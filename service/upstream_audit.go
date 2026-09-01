package service

import (
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
)

// ── upstream response model auditing ─────────────────────────────────────────
// Compares the model we sent upstream with the model the upstream response
// declared. Pure observation: never touches the forwarding or billing path.
// Design mirrors sub2api's upstream_response_model.go (tri-state mismatch,
// vendor alias canonicalization).

// canonicalGrokRuntimeModel maps xAI public aliases onto the runtime build id
// the API reports, so alias↔build echoes don't count as mismatches.
func canonicalGrokRuntimeModel(m string) string {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case "grok-4.5", "grok-4.5-latest", "grok-4.5-build":
		return "grok-4.5-build"
	case "grok-4.6", "grok-4.6-latest", "grok-4.6-build":
		return "grok-4.6-build"
	default:
		return ""
	}
}

// UpstreamModelsMatchForAudit reports whether the upstream-declared model is
// consistent with the model we sent. Case-insensitive; known vendor runtime
// aliases are canonicalized before comparing.
func UpstreamModelsMatchForAudit(sentModel, responseModel string) bool {
	sentModel = strings.TrimSpace(sentModel)
	responseModel = strings.TrimSpace(responseModel)
	if sentModel == "" || responseModel == "" {
		return true // nothing to compare — never flag on missing data
	}
	if strings.EqualFold(sentModel, responseModel) {
		return true
	}
	if g := canonicalGrokRuntimeModel(sentModel); g != "" && g == canonicalGrokRuntimeModel(responseModel) {
		return true
	}
	return false
}

// appendUpstreamResponseAudit adds the ursp block to other.admin_info and, on
// mismatch, persists the full forensic evidence asynchronously.
func appendUpstreamResponseAudit(relayInfo *relaycommon.RelayInfo, adminInfo map[string]interface{}) {
	if relayInfo == nil || adminInfo == nil {
		return
	}
	respModel := relayInfo.UpstreamResponseModel
	if respModel == "" {
		return // tri-state: upstream never declared a model — record nothing
	}
	sentModel := relayInfo.UpstreamSentModel
	if sentModel == "" {
		sentModel = relayInfo.UpstreamModelName
	}
	mismatch := !UpstreamModelsMatchForAudit(sentModel, respModel)

	ursp := map[string]interface{}{
		"model":    respModel,
		"sent":     sentModel,
		"mismatch": mismatch,
	}
	if relayInfo.UpstreamResponseId != "" {
		ursp["id"] = relayInfo.UpstreamResponseId
	}
	if relayInfo.UpstreamSystemFingerprint != "" {
		ursp["sf"] = relayInfo.UpstreamSystemFingerprint
	}
	if relayInfo.UpstreamResponseCreated != 0 {
		ursp["created"] = relayInfo.UpstreamResponseCreated
	}
	adminInfo["ursp"] = ursp

	if !mismatch {
		return
	}

	// evidence snapshot — copy everything needed before leaving the request scope
	ev := &model.UpstreamEvidence{
		CreatedAt:         common.GetTimestamp(),
		UserId:            relayInfo.UserId,
		TokenId:           relayInfo.TokenId,
		RequestModel:      relayInfo.OriginModelName,
		SentModel:         sentModel,
		ResponseModel:     respModel,
		ResponseId:        relayInfo.UpstreamResponseId,
		SystemFingerprint: relayInfo.UpstreamSystemFingerprint,
		ResponseCreated:   relayInfo.UpstreamResponseCreated,
		IsStream:          relayInfo.IsStream,
		UseTimeMs:         int(time.Since(relayInfo.StartTime).Milliseconds()),
		ResponseExcerpt:   relayInfo.UpstreamResponseExcerpt,
		ResponseHeaders:   relayInfo.UpstreamResponseHeaders,
	}
	if relayInfo.ChannelMeta != nil {
		ev.ChannelId = relayInfo.ChannelMeta.ChannelId
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				common.SysError("upstream evidence persist panic recovered")
			}
		}()
		if err := model.InsertUpstreamEvidence(ev); err != nil {
			common.SysError("upstream evidence persist failed: " + err.Error())
		} else {
			common.SysLog("[upstream-audit] model mismatch recorded: sent=" + ev.SentModel +
				" got=" + ev.ResponseModel)
		}
	}()
}
