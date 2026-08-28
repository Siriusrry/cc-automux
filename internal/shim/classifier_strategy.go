package shim

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// classifierProfileKind selects which proven rewrite/response path an auto-mode
// classifier request takes. The profile is chosen by the request model's family
// rather than by the URL prefix, so a GPT classifier carried on /any or a Claude
// classifier carried on /cpa each get the rewrite that backend expects.
type classifierProfileKind int

const (
	anthropicClassifierProfile classifierProfileKind = iota
	openAIClassifierProfile
)

// classifierProfile maps a model name to the profile its classifier requests
// need: OpenAI families (gpt*, and the o-series o<digit>* — o1, o2, o3, o4,
// o5, …) use the CLIProxyAPI-style request rewrite plus GPT response
// reassembly; everything else (claude*, and unknown models) defaults to the
// anthropic profile.
func classifierProfile(model string) classifierProfileKind {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(m, "gpt"), isOpenAIOSeries(m):
		return openAIClassifierProfile
	default:
		return anthropicClassifierProfile
	}
}

// isOpenAIOSeries reports whether a model name is an OpenAI o-series model: an
// "o" immediately followed by a digit (o1, o2, o3, o4, o5, …), so new o-series
// releases map to openai without needing a per-number branch.
func isOpenAIOSeries(m string) bool {
	return len(m) >= 2 && m[0] == 'o' && m[1] >= '0' && m[1] <= '9'
}

// classifierRequestModel returns the model a classifier request will run on:
// the configured override when set, otherwise the body's own model.
func classifierRequestModel(body map[string]any, override string) string {
	if override != "" {
		return override
	}
	model, _ := body["model"].(string)
	return model
}

// classifierProfileStrategy is the proxyStrategy used for a detected auto-mode
// classifier request once dispatch has selected its profile by model family. It
// reuses the existing AnyRouter / CLIProxyAPI rewrite and response code, but the
// upstream-platform adaptations are chosen ORTHOGONALLY — by the resolved upstream
// platform ⊗ the model-family profile — rather than by the URL prefix the
// request arrived on. It also supports an optional model-name override.
type classifierProfileStrategy struct {
	profile       classifierProfileKind
	modelOverride string
	// platform is the resolved upstream platform of this classifier request's
	// destination: the matched prefix's backend by default, or the global target's
	// resolved target_type when one is configured (set by dispatchClassifier). It
	// decides each upstream adaptation orthogonally to the model-family profile:
	//
	//   - CPA session isolation (new session UUID + session header) ⟺
	//     profile == openAIClassifierProfile && platform == platformCPA. Isolation's
	//     real cause is CPA's codex_executor session-keyed cache (a gpt/codex
	//     backend quirk), so it is gated on the OPENAI profile AND a CPA platform. A
	//     claude classifier (anthropic profile) is NEVER isolated on any platform,
	//     including CPA — the anthropic branch below never touches the session.
	//
	//   - AnyRouter identity-marker cloak + delete thinking.disabled ⟺
	//     profile == anthropicClassifierProfile && platform == platformAnyRouter.
	//     That rewrite is an AnyRouter strict-path quirk (its cloak path requires the
	//     first system block to be the identity marker and rejects thinking.disabled),
	//     not a claude-model trait, so it applies only when a claude classifier is
	//     actually destined for AnyRouter. A claude classifier to any other platform
	//     (generic / CPA) is forwarded unchanged — claude format needs no
	//     request-side rewrite — and streamed.
	//
	//     Scope boundary (deliberate): the OPENAI profile never cloaks on ANY
	//     platform including AnyRouter — the cloak is a claude-identity strict-path
	//     adaptation, not a generic per-platform step. A GPT classifier on AnyRouter
	//     goes through the reassembly path and is verified to correctly need no cloak
	//     because the identity marker is specific to Claude traffic.
	//
	//   - gpt response reassembly + strip Accept-Encoding ⟺
	//     profile == openAIClassifierProfile (EVERY platform — the openai profile
	//     always reassembles by JSON-parsing the body, so it must be uncompressed).
	//
	//   - otherwise (anthropic profile on a non-AnyRouter platform — generic / CPA
	//     claude) → forwarded unchanged and streamed.
	platform classifierPlatform
}

func (s classifierProfileStrategy) rewriteRequest(_ *http.Request, _ string, raw []byte) ([]byte, requestContext, error) {
	raw, err := applyClassifierModelOverride(raw, s.modelOverride)
	if err != nil {
		return nil, requestContext{}, err
	}
	if s.profile == openAIClassifierProfile {
		// Session isolation is applied only when this gpt classifier's destination
		// platform is CPA; on any other platform the openai profile still reassembles
		// but leaves the session untouched.
		return rewriteOpenAIClassifier(raw, s.platform == platformCPA)
	}
	// anthropic profile. The identity-marker cloak + delete thinking.disabled is an
	// AnyRouter strict-path quirk, so apply it only when the destination platform is
	// AnyRouter; to any other platform a claude-format classifier is forwarded
	// unchanged (no request-side rewrite) and streamed.
	if s.platform != platformAnyRouter {
		return raw, requestContext{}, nil
	}
	body, rewritten, err := rewriteAnyRouterClassifier(raw)
	if err != nil {
		return raw, requestContext{}, err
	}
	return body, requestContext{rewritten: rewritten, classifier: rewritten}, nil
}

func (s classifierProfileStrategy) applyRequestHeaders(dst, src http.Header, ctx requestContext) {
	copyRequestHeaders(dst, src)
	if s.profile == openAIClassifierProfile && ctx.classifier {
		// The openai profile always reassembles the response by JSON-parsing the
		// body, so force an uncompressed classifier response on every platform
		// (AnyRouter / generic global target as well as CPA). The non-CPA clients set
		// DisableCompression, so an explicitly forwarded Accept-Encoding: gzip
		// would not be auto-decoded and reassembly would fail on the gzip bytes.
		dst.Del("Accept-Encoding")
		if s.platform == platformCPA {
			// Session isolation stays CPA-only.
			dst.Set(sessionHeader, ctx.sessionID)
		}
	}
}

func (s classifierProfileStrategy) writeResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, ctx requestContext) error {
	if s.profile == openAIClassifierProfile {
		return writeReassembledClassifierResponse("classifier", w, r, resp, ctx)
	}
	return streamResponse(w, resp)
}

// rewriteOpenAIClassifier applies the CLIProxyAPI-style classifier request
// rewrite: it always captures stop_sequences (the response reassembler needs
// them) and re-marshals the body, and rewrites the session id only when the
// request is destined for CPA. It mirrors cliproxyStrategy.rewriteRequest for
// the isolating case.
func rewriteOpenAIClassifier(raw []byte, isolate bool) ([]byte, requestContext, error) {
	body, ok, err := parseAutoModeClassifier(raw)
	if err != nil || !ok {
		return raw, requestContext{}, err
	}
	ctx := requestContext{
		rewritten:     true,
		classifier:    true,
		stopSequences: extractStopSequences(body),
	}
	if isolate {
		sid, err := newUUID()
		if err != nil {
			return nil, requestContext{}, err
		}
		rewriteSessionInBody(body, sid)
		ctx.sessionID = sid
	}
	newBody, err := marshalJSON(body)
	if err != nil {
		return nil, requestContext{}, err
	}
	return newBody, ctx, nil
}

// applyClassifierModelOverride overwrites the request model when an override is
// configured. An empty override leaves the body untouched (no parse), and a body
// that does not decode is passed through unchanged so the profile rewrite can
// surface its own error.
func applyClassifierModelOverride(raw []byte, override string) ([]byte, error) {
	if override == "" {
		return raw, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var body map[string]any
	if err := decoder.Decode(&body); err != nil {
		return raw, nil
	}
	body["model"] = override
	return marshalJSON(body)
}
