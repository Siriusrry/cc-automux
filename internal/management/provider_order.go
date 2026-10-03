package management

import (
	"net/http"
	"sort"
	"strconv"

	"github.com/Siriusrry/cc-automux/internal/config"
)

func (h *Handler) handleProviderOrder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		methodNotAllowed(w, http.MethodPut)
		return
	}
	body, err := readBody(w, r, h.maxBodyBytes)
	if err != nil {
		return
	}
	update, err := config.DecodeProviderOrder(body)
	if err != nil {
		h.writeDecodeError(w, err)
		return
	}
	result, err := h.manager.ReorderProviders(update.ProviderIDs, r.Header.Get("If-Match"))
	if err != nil {
		h.writeApplyError(w, err)
		return
	}
	h.writeApplyResult(w, result)
}

type providerTierResponse struct {
	Priority    string   `json:"priority"`
	ProviderIDs []string `json:"provider_ids"`
}

func providerTiers(providers []config.ProviderConfig) []providerTierResponse {
	groups := make(map[int64][]string)
	for _, p := range providers {
		groups[p.Priority] = append(groups[p.Priority], p.ID)
	}
	priorities := make([]int64, 0, len(groups))
	for priority := range groups {
		priorities = append(priorities, priority)
	}
	sort.Slice(priorities, func(i, j int) bool { return priorities[i] > priorities[j] })
	tiers := make([]providerTierResponse, 0, len(priorities))
	for _, priority := range priorities {
		tiers = append(tiers, providerTierResponse{Priority: strconv.FormatInt(priority, 10), ProviderIDs: groups[priority]})
	}
	return tiers
}
