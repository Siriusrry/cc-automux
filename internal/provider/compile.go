package provider

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"os"

	"github.com/Siriusrry/cc-automux/internal/config"
	"github.com/Siriusrry/cc-automux/internal/patch"
)

// CompiledTarget contains the immutable request-affecting part shared by pool
// providers and a fixed classifier target. Pool providers use it today, and
// compiling this boundary prevents a second patch/transport implementation.
type CompiledTarget struct {
	ID          string
	BaseURL     *url.URL
	APIKey      string
	UseXAPIKey  bool
	TLS         *tls.Config
	TLSSettings config.TLSConfig
	PatchPlan   patch.Plan
	Generation  ProviderGeneration
}

// FixedTargetID is the stable internal identity used for a pool-external
// classifier target.  It is deliberately not configurable and fixed targets
// never enter the Provider Catalog, Scheduler, Health, or sticky assignments.
const FixedTargetID = "auto-mode-fixed-provider"

// CompiledFixedTarget is the runtime-ready fixed classifier target.  Protocol
// belongs only to this pool-external target; ordinary pool targets cannot carry
// a protocol identifier by construction.
type CompiledFixedTarget struct {
	CompiledTarget
	Protocol string
}

// Clone returns a defensive copy of a compiled target. Patch plans retain the
// immutable registry definitions and can be copied by value; URL and TLS
// pointers are cloned so callers cannot mutate the target through them.
func (t *CompiledTarget) Clone() *CompiledTarget {
	if t == nil {
		return nil
	}
	out := *t
	if t.BaseURL != nil {
		urlCopy := *t.BaseURL
		out.BaseURL = &urlCopy
	}
	if t.TLS != nil {
		out.TLS = t.TLS.Clone()
	}
	return &out
}

// Clone returns a defensive copy of a fixed target, including its protocol
// identifier and the embedded common target.
func (t *CompiledFixedTarget) Clone() *CompiledFixedTarget {
	if t == nil {
		return nil
	}
	return &CompiledFixedTarget{
		CompiledTarget: *t.CompiledTarget.Clone(),
		Protocol:       t.Protocol,
	}
}

// RuntimeContext is the explicit, application-owned context required to
// compile providers.  It carries the one immutable Patch Registry and its
// process-local shared services (including the AliasStore).  Provider
// compilation never creates a registry or session map on its own; the
// composition root creates this context once and Runtime Manager reuses it
// for every snapshot.
type RuntimeContext struct {
	Registry patch.Registry
}

// NewRuntimeContext validates and freezes the caller-supplied runtime
// resources.  The registry value contains handles to shared services; those
// handles are intentionally not cloned.
func NewRuntimeContext(registry patch.Registry) (RuntimeContext, error) {
	if registry.Empty() {
		return RuntimeContext{}, errors.New("patch registry is required")
	}
	if registry.RequiresAliasStore() && registry.AliasStore() == nil {
		return RuntimeContext{}, errors.New("alias store service is required by the patch registry")
	}
	return RuntimeContext{Registry: registry}, nil
}

func (c RuntimeContext) Validate() error {
	if c.Registry.Empty() {
		return errors.New("patch registry is required")
	}
	if c.Registry.RequiresAliasStore() && c.Registry.AliasStore() == nil {
		return errors.New("alias store service is required by the patch registry")
	}
	return nil
}

// CompiledProvider is the immutable, runtime-ready form of one configured
// provider. It contains only static data; health and scheduling state are not
// stored here.
type CompiledProvider struct {
	CompiledTarget
	Name          string
	Models        []string
	Priority      int64
	Enabled       bool
	Patches       []patch.PatchMetadata
	DisableHealth bool

	modelSet map[string]struct{}
}

// Compile validates and compiles one provider using the caller-supplied,
// process-owned RuntimeContext. Every discoverable patch is executable;
// unknown IDs fail before a runtime snapshot can be published. No implicit
// registry or AliasStore is created here.
func Compile(input config.ProviderConfig, context RuntimeContext) (*CompiledProvider, error) {
	if err := context.Validate(); err != nil {
		return nil, err
	}
	return compileWithRegistry(input, context.Registry)
}

// CompileFixedTarget validates and compiles one pool-external classifier
// target using the caller-owned RuntimeContext.  The classifier model is
// supplied separately from the target object so it cannot be duplicated in
// persisted configuration.  The target's patch plan is filtered to patches
// applicable to classifier requests; normal-only IDs fail closed.
func CompileFixedTarget(input config.FixedProviderConfig, classifierModel string, context RuntimeContext) (*CompiledFixedTarget, error) {
	if err := context.Validate(); err != nil {
		return nil, err
	}
	if err := validateFixedTargetInput(input, classifierModel); err != nil {
		return nil, err
	}
	parsed, err := url.Parse(input.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("base_url: %w", err)
	}
	tlsConfig, err := compileTLS(input.TLS)
	if err != nil {
		return nil, fmt.Errorf("tls: %w", err)
	}
	plan, err := context.Registry.Compile(input.Patches, patch.RequestTypeClassifier)
	if err != nil {
		return nil, err
	}
	return &CompiledFixedTarget{
		CompiledTarget: CompiledTarget{
			ID:          FixedTargetID,
			BaseURL:     parsed,
			APIKey:      input.APIKey,
			UseXAPIKey:  input.UseXAPIKey,
			TLS:         tlsConfig,
			TLSSettings: input.TLS,
			PatchPlan:   plan,
			Generation:  fixedGenerationFor(input, classifierModel),
		},
		Protocol: input.Protocol,
	}, nil
}

// CompileAutoModeTarget is a convenience wrapper for Runtime Manager callers
// that already hold the complete validated Auto Mode value.  Disabled and
// provider-pool modes intentionally produce no fixed target.
func CompileAutoModeTarget(auto config.AutoModeConfig, context RuntimeContext) (*CompiledFixedTarget, error) {
	auto = auto.Normalize()
	if err := auto.Validate(); err != nil {
		return nil, err
	}
	if auto.Mode != config.AutoModeFixedProvider {
		return nil, nil
	}
	if auto.FixedProvider == nil {
		return nil, errors.New("auto_mode.fixed_provider is required")
	}
	return CompileFixedTarget(*auto.FixedProvider, auto.Model, context)
}

func validateFixedTargetInput(input config.FixedProviderConfig, classifierModel string) error {
	if err := input.Validate(); err != nil {
		return err
	}
	return config.AutoModeConfig{Mode: config.AutoModeProviderPool, Model: classifierModel}.Validate()
}

func compileWithRegistry(input config.ProviderConfig, registry patch.Registry) (*CompiledProvider, error) {
	// The config package owns the schema-level provider rules. Wrapping this one
	// provider in a complete configuration keeps the two packages from duplicating
	// validation logic while still allowing the provider compiler to be used on
	// its own.
	check := config.Default()
	check.Auth.ManagementKey = "provider-compiler-validation-key"
	check.Providers = []config.ProviderConfig{input}
	if err := check.Validate(); err != nil {
		return nil, err
	}
	parsed, err := url.Parse(input.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("base_url: %w", err)
	}
	tlsConfig, err := compileTLS(input.TLS)
	if err != nil {
		return nil, fmt.Errorf("tls: %w", err)
	}
	plan, err := registry.Compile(input.Patches)
	if err != nil {
		return nil, err
	}
	patches := plan.List()
	models := append([]string(nil), input.Models...)
	modelSet := make(map[string]struct{}, len(models))
	for _, model := range models {
		modelSet[model] = struct{}{}
	}
	generation := generationFor(input)
	return &CompiledProvider{
		CompiledTarget: CompiledTarget{
			ID:          input.ID,
			BaseURL:     parsed,
			APIKey:      input.APIKey,
			UseXAPIKey:  input.UseXAPIKey,
			TLS:         tlsConfig,
			TLSSettings: input.TLS,
			Generation:  generation,
			PatchPlan:   plan,
		},
		Name:          input.Name,
		Models:        models,
		Priority:      input.Priority,
		Enabled:       input.Enabled,
		Patches:       patches,
		DisableHealth: input.DisableHealth,
		modelSet:      modelSet,
	}, nil
}

// CompileCatalog compiles every provider and builds an exact, case-sensitive
// model index. The returned catalog owns all slices and maps.
func CompileCatalog(inputs []config.ProviderConfig, context RuntimeContext) (*Catalog, error) {
	if err := context.Validate(); err != nil {
		return nil, err
	}
	return compileCatalogWithRegistry(inputs, context.Registry)
}

func compileCatalogWithRegistry(inputs []config.ProviderConfig, registry patch.Registry) (*Catalog, error) {
	// Validate root-level duplicate IDs/names and auth-independent schema rules
	// before compiling. A complete config also catches duplicate models.
	check := config.Default()
	check.Auth.ManagementKey = "provider-catalog-validation-key"
	check.Providers = append([]config.ProviderConfig(nil), inputs...)
	if err := check.Validate(); err != nil {
		return nil, err
	}
	providers := make([]*CompiledProvider, 0, len(inputs))
	index := make(map[string][]*CompiledProvider)
	for _, input := range inputs {
		compiled, err := compileWithRegistry(input, registry)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", input.ID, err)
		}
		providers = append(providers, compiled)
		if !compiled.Enabled || len(compiled.Models) == 0 {
			continue
		}
		for _, model := range compiled.Models {
			index[model] = append(index[model], compiled)
		}
	}
	for model, candidates := range index {
		stablePrioritySort(candidates)
		index[model] = candidates
	}
	return &Catalog{providers: providers, index: index}, nil
}

func stablePrioritySort(items []*CompiledProvider) {
	for i := 1; i < len(items); i++ {
		current := items[i]
		j := i
		for j > 0 && items[j-1].Priority < current.Priority {
			items[j] = items[j-1]
			j--
		}
		items[j] = current
	}
}

func compileTLS(input config.TLSConfig) (*tls.Config, error) {
	if input.CAFile != "" && input.InsecureSkipVerify {
		return nil, errors.New("ca_file and insecure_skip_verify are mutually exclusive")
	}
	if input.CAFile == "" {
		if input.InsecureSkipVerify {
			return &tls.Config{InsecureSkipVerify: true}, nil // explicitly requested by config
		}
		// nil delegates to the Go transport's system certificate pool.
		return nil, nil
	}
	pemBytes, err := os.ReadFile(input.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read CA file: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, errors.New("CA file contains no certificates")
	}
	return &tls.Config{RootCAs: pool}, nil
}

// Clone returns a defensive copy suitable for an API or snapshot boundary.
func (p *CompiledProvider) Clone() *CompiledProvider {
	if p == nil {
		return nil
	}
	out := *p
	out.Models = append([]string(nil), p.Models...)
	out.Patches = p.PatchPlan.List()
	out.CompiledTarget = p.CompiledTarget
	if p.CompiledTarget.BaseURL != nil {
		urlCopy := *p.CompiledTarget.BaseURL
		out.CompiledTarget.BaseURL = &urlCopy
	}
	if p.CompiledTarget.TLS != nil {
		out.CompiledTarget.TLS = p.CompiledTarget.TLS.Clone()
	}
	// TLSSettings contains only strings and a bool, so the value copy is enough.
	out.modelSet = make(map[string]struct{}, len(p.modelSet))
	for model := range p.modelSet {
		out.modelSet[model] = struct{}{}
	}
	return &out
}

func (p *CompiledProvider) SupportsModel(model string) bool {
	if p == nil || !p.Enabled {
		return false
	}
	_, ok := p.modelSet[model]
	return ok
}

func (t *CompiledTarget) URLString() string {
	if t == nil || t.BaseURL == nil {
		return ""
	}
	return t.BaseURL.String()
}

// TLSClone returns a client-safe copy. A nil result means system roots.
func (t *CompiledTarget) TLSClone() *tls.Config {
	if t == nil || t.TLS == nil {
		return nil
	}
	return t.TLS.Clone()
}

// Config returns a defensive config representation of the compiled provider.
func (p *CompiledProvider) Config() config.ProviderConfig {
	if p == nil {
		return config.ProviderConfig{}
	}
	out := config.ProviderConfig{
		ID:            p.ID,
		Name:          p.Name,
		BaseURL:       p.URLString(),
		APIKey:        p.APIKey,
		Models:        append([]string(nil), p.Models...),
		Priority:      p.Priority,
		Enabled:       p.Enabled,
		UseXAPIKey:    p.UseXAPIKey,
		TLS:           p.TLSSettings,
		Patches:       make([]string, len(p.Patches)),
		DisableHealth: p.DisableHealth,
	}
	for i, patch := range p.Patches {
		out.Patches[i] = patch.ID
	}
	return out
}

func (p *CompiledProvider) String() string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprintf("provider{id=%s,name=%s,url=%s}", p.ID, p.Name, p.URLString())
}
