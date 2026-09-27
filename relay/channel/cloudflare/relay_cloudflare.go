package cloudflare

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/samber/lo"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func convertCf2CompletionsRequest(textRequest dto.GeneralOpenAIRequest) *CfRequest {
	p, _ := textRequest.Prompt.(string)
	return &CfRequest{
		Prompt:      p,
		MaxTokens:   textRequest.GetMaxTokens(),
		Stream:      lo.FromPtrOr(textRequest.Stream, false),
		Temperature: textRequest.Temperature,
	}
}

func cfStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*types.NewAPIError, *dto.Usage) {
	scanner := helper.NewStreamScanner(resp.Body)
	scanner.Split(bufio.ScanLines)

	helper.SetEventStreamHeaders(c)
	info.MarkUpstreamObserver("cloudflare_stream")
	info.SetUpstreamResponseForensics("", resp.Header)
	id := helper.GetResponseID(c)
	var responseText string
	isFirst := true
	var lastStreamData string

	for scanner.Scan() {
		data := scanner.Text()
		if len(data) < len("data: ") {
			continue
		}
		data = strings.TrimPrefix(data, "data: ")
		data = strings.TrimSuffix(data, "\r")

		if data == "[DONE]" {
			break
		}

		var response dto.ChatCompletionsStreamResponse
		err := json.Unmarshal([]byte(data), &response)
		if err != nil {
			logger.LogError(c, "error_unmarshalling_stream_response: "+err.Error())
			continue
		}
		// upstream response forensics — before the id/model are overwritten below
		info.ObserveUpstreamResponseFrame(data, response.Model, response.Id, response.Created, "")
		lastStreamData = data
		for _, choice := range response.Choices {
			choice.Delta.Role = "assistant"
			responseText += choice.Delta.GetContentString()
		}
		response.Id = id
		response.Model = info.UpstreamModelName
		err = helper.ObjectData(c, response)
		if isFirst {
			isFirst = false
			info.FirstResponseTime = time.Now()
		}
		if err != nil {
			logger.LogError(c, "error_rendering_stream_response: "+err.Error())
		}
	}

	if err := scanner.Err(); err != nil {
		logger.LogError(c, "error_scanning_stream_response: "+err.Error())
	}
	info.SetUpstreamResponseForensics(lastStreamData, nil)
	info.ObserveUpstreamOutput(responseText)
	usage := service.ResponseText2Usage(c, responseText, info.UpstreamModelName, info.GetEstimatePromptTokens())
	if info.ShouldIncludeUsage {
		response := helper.GenerateFinalUsageResponse(id, info.StartTime.Unix(), info.UpstreamModelName, *usage)
		err := helper.ObjectData(c, response)
		if err != nil {
			logger.LogError(c, "error_rendering_final_usage_response: "+err.Error())
		}
	}
	helper.Done(c)

	service.CloseResponseBodyGracefully(resp)

	return nil, usage
}

func cfHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*types.NewAPIError, *dto.Usage) {
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return types.NewError(err, types.ErrorCodeBadResponseBody), nil
	}
	service.CloseResponseBodyGracefully(resp)
	// upstream response forensics — before the id/model are overwritten below
	info.MarkUpstreamObserver("cloudflare")
	info.SetUpstreamResponseForensicsBytes(responseBody, resp.Header)
	info.ObserveUpstreamResponse(gjson.GetBytes(responseBody, "model").String(), gjson.GetBytes(responseBody, "id").String(), gjson.GetBytes(responseBody, "created").Int(), "")
	info.ObserveUpstreamOutputJSON(responseBody)
	var response dto.TextResponse
	err = json.Unmarshal(responseBody, &response)
	if err != nil {
		return types.NewError(err, types.ErrorCodeBadResponseBody), nil
	}
	response.Model = info.UpstreamModelName
	var responseText string
	for _, choice := range response.Choices {
		responseText += choice.Message.StringContent()
	}
	usage := service.ResponseText2Usage(c, responseText, info.UpstreamModelName, info.GetEstimatePromptTokens())
	response.Usage = *usage
	response.Id = helper.GetResponseID(c)
	jsonResponse, err := json.Marshal(response)
	if err != nil {
		return types.NewError(err, types.ErrorCodeBadResponseBody), nil
	}
	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(resp.StatusCode)
	_, _ = c.Writer.Write(jsonResponse)
	return nil, usage
}

func cfSTTHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*types.NewAPIError, *dto.Usage) {
	var cfResp CfAudioResponse
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return types.NewError(err, types.ErrorCodeBadResponseBody), nil
	}
	service.CloseResponseBodyGracefully(resp)
	err = json.Unmarshal(responseBody, &cfResp)
	if err != nil {
		return types.NewError(err, types.ErrorCodeBadResponseBody), nil
	}

	audioResp := &dto.AudioResponse{
		Text: cfResp.Result.Text,
	}

	jsonResponse, err := json.Marshal(audioResp)
	if err != nil {
		return types.NewError(err, types.ErrorCodeBadResponseBody), nil
	}
	c.Writer.Header().Set("Content-Type", "application/json")
	c.Writer.WriteHeader(resp.StatusCode)
	_, _ = c.Writer.Write(jsonResponse)

	usage := service.ResponseText2Usage(c, cfResp.Result.Text, info.UpstreamModelName, info.GetEstimatePromptTokens())
	return nil, usage
}
