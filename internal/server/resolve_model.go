package server

import (
	"strings"

	"qoder2api/internal/upstream"
)

// resolveModel parses optional realm prefix ("cn:...", "global:...", "intl:...", "cn/...", "gl/...")
// or infers exclusive Qoder realm (e.g. smodel/cmodel/ultimate -> global; q37fmodel/gm51model -> cn).
// When unspecified and shared across both realms, defaults to "global" if GlobalEnabled else "cn".
func resolveModel(model string) (realm, bare string) {
	exp, bareStr := upstream.SplitRealmPrefix(model)
	bare = bareStr
	if exp != "" {
		return exp, bare
	}
	if autoR, _ := upstream.AutoRealmForModel(bare); autoR != "" {
		return autoR, bare
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(bare)), "dsh") {
		return "global", bare
	}
	return "cn", bare
}

// ResolveModel exports resolveModel for sibling wiring packages.
func ResolveModel(model string) (realm, bare string) { return resolveModel(model) }

// resolveModelForHandler uses handler configuration (GlobalEnabled + pool availability + aliases)
// to resolve both realm and canonical Qoder model key cleanly.
func (h *Handler) resolveModelForHandler(model string) (realm, bare string) {
	reqRealm, reqBare := resolveModel(model)
	expPrefix, _ := upstream.SplitRealmPrefix(model)
	lowerBare := strings.ToLower(strings.TrimSpace(reqBare))

	if h != nil && h.cfg.ModelAliases != nil {
		if target, ok := h.cfg.ModelAliases[model]; ok && target != "" {
			return h.resolveModelForHandler(target)
		}
		if target, ok := h.cfg.ModelAliases[strings.ToLower(strings.TrimSpace(model))]; ok && target != "" {
			return h.resolveModelForHandler(target)
		}
		if target, ok := h.cfg.ModelAliases[reqBare]; ok && target != "" {
			reqBare = target
		} else if target, ok := h.cfg.ModelAliases[lowerBare]; ok && target != "" {
			reqBare = target
		}
	}

	resolvedKey := upstream.ResolveUpstreamKey(reqBare, reqRealm)
	if expPrefix == "" {
		if autoR, _ := upstream.AutoRealmForModel(resolvedKey); autoR != "" {
			return autoR, resolvedKey
		}
		// Shared model across CN and Global: prefer whichever realm currently has ready accounts if CN has none.
		if h != nil && h.cfg.GlobalEnabled && h.cfg.Pool != nil {
			cnAvail := len(h.cfg.Pool.AvailableUIDsForRealm("cn"))
			glAvail := len(h.cfg.Pool.AvailableUIDsForRealm("global"))
			if cnAvail == 0 && glAvail > 0 {
				return "global", resolvedKey
			}
		}
	}
	return reqRealm, resolvedKey
}
