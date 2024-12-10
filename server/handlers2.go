package sentinel

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/sashabaranov/go-openai"
)

func apiGetToolCallHandler(w http.ResponseWriter, r *http.Request, id uuid.UUID, store Store) {
	toolCall, err := store.GetToolCall(r.Context(), id)
	if err != nil {
		sendErrorResponse(w, http.StatusInternalServerError, "error getting tool call", err.Error())
		return
	}

	if toolCall == nil {
		sendErrorResponse(w, http.StatusNotFound, "tool call not found", "")
		return
	}

	respondJSON(w, toolCall, http.StatusOK)
}

func apiCreateNewChatHandler(w http.ResponseWriter, r *http.Request, runId uuid.UUID, store Store) {
	ctx := r.Context()

	var payload SentinelChat
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		sendErrorResponse(w, http.StatusBadRequest, "Invalid JSON format", err.Error())
		return
	}

	jsonRequest, requestMessages, err := validateAndDecodeRequest(ctx, payload.RequestData, runId, store)
	if err != nil {
		sendErrorResponse(w, http.StatusBadRequest, fmt.Sprintf("Request: %s", err.Error()), "")
		return
	}

	// len(requestMessages) should be the same as the total number of messages in msg for this run
	// (exluding unpicked choices)

	newRequestMessages, err := filterRequestMessages(ctx, requestMessages, runId, store)
	if err != nil {
		sendErrorResponse(w, http.StatusBadRequest, fmt.Sprintf("Error filtering request messages: %s", err.Error()), "")
		return
	}

	jsonResponse, response, err := validateAndDecodeResponse(payload.ResponseData)
	if err != nil {
		sendErrorResponse(w, http.StatusBadRequest, fmt.Sprintf("Response: %s", err.Error()), "")
		return
	}

	// Parse out the choices into SentinelChoice objects
	choices, err := convertChoices(ctx, response.Choices, runId, store)
	if err != nil {
		sendErrorResponse(w, http.StatusBadRequest, fmt.Sprintf("Error converting choices: %s", err.Error()), "")
		return
	}

	id, err := store.CreateChatRequest(ctx, runId, jsonRequest, jsonResponse, choices, "openai", newRequestMessages)
	if err != nil {
		sendErrorResponse(w, http.StatusBadRequest, fmt.Sprintf("Error creating chat request: %s", err.Error()), "")
		return
	}

	// Extract all IDs from the created chat structure
	chatIds := extractChatIds(*id, choices)

	respondJSON(w, chatIds, http.StatusOK)
}

// filterRequestMessages filters out the request messages that are not new in this request
// by cutting off the first n messages, where n is the number of messages logged against choices in
// this run already.
func filterRequestMessages(ctx context.Context, requestMessages []SentinelMessage, runId uuid.UUID, store Store) ([]SentinelMessage, error) {
	messagesForRun, err := store.GetMessagesForRun(ctx, runId)
	if err != nil {
		return nil, fmt.Errorf("error getting messages for run: %w", err)
	}

	// Get the number of messages logged against choices in this run
	numMessagesLogged := len(messagesForRun)

	// Cut off the first n messages
	newRequestMessages := requestMessages[numMessagesLogged:]

	return newRequestMessages, nil
}

// validateAndDecodeRequest handles the decoding and validation of the chat completion request
// It splits out the messages and converts them to SentinelMessage objects
func validateAndDecodeRequest(ctx context.Context, encodedData string, runId uuid.UUID, store ToolStore) ([]byte, []SentinelMessage, error) {
	decodedRequest, err := base64.StdEncoding.DecodeString(encodedData)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid base64 format: %w", err)
	}

	var v openai.ChatCompletionRequest
	if err = json.Unmarshal(decodedRequest, &v); err != nil {
		return nil, nil, fmt.Errorf("invalid request format: %w", err)
	}

	// Extract messages from the request
	messages := v.Messages
	convertedMessages := make([]SentinelMessage, 0, len(messages))
	for _, message := range messages {
		fmt.Printf("Message in OpenAI format: %+v\n", message)
		convertedMessage, err := convertMessage(ctx, message, runId, store)
		if err != nil {
			return nil, nil, fmt.Errorf("error converting messages: %w", err)
		}
		convertedMessages = append(convertedMessages, convertedMessage)
	}

	marshaledRequest, err := json.Marshal(v)
	if err != nil {
		return nil, nil, fmt.Errorf("error marshalling request: %w", err)
	}

	return marshaledRequest, convertedMessages, nil
}

// validateAndDecodeResponse handles the decoding and validation of the chat completion response
func validateAndDecodeResponse(encodedData string) ([]byte, *openai.ChatCompletionResponse, error) {
	decodedResponse, err := base64.StdEncoding.DecodeString(encodedData)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid base64 format: %w", err)
	}

	var v openai.ChatCompletionResponse
	if err = json.Unmarshal(decodedResponse, &v); err != nil {
		return nil, nil, fmt.Errorf("invalid response format: %w", err)
	}

	b, err := json.Marshal(v)
	if err != nil {
		return nil, nil, fmt.Errorf("error marshalling response: %w", err)
	}

	return b, &v, nil
}

func convertChoices(ctx context.Context, choices []openai.ChatCompletionChoice, runId uuid.UUID, store ToolStore) ([]SentinelChoice, error) {
	var result []SentinelChoice
	for _, choice := range choices {
		message, err := convertMessage(ctx, choice.Message, runId, store)
		if err != nil {
			return nil, fmt.Errorf("error converting message: %w", err)
		}

		id := uuid.New().String()
		result = append(result, SentinelChoice{
			SentinelId:   id,
			Index:        choice.Index,
			Message:      message,
			FinishReason: SentinelChoiceFinishReason(choice.FinishReason),
		})
	}

	return result, nil
}

func convertMessage(ctx context.Context, message openai.ChatCompletionMessage, runId uuid.UUID, store ToolStore) (SentinelMessage, error) {
	toolCalls, err := convertToolCalls(ctx, message.ToolCalls, runId, store)
	if err != nil {
		return SentinelMessage{}, fmt.Errorf("error converting tool calls: %w", err)
	}

	// If the message has an image in it, it will look like this:
	// {Role:user Content: Refusal: MultiContent:[{Type:image_url Text: ImageURL:0xc000220320}] Name: FunctionCall:<nil> ToolCalls:[] ToolCallID:}
	// We need to convert this to a SentinelMessage with a type of ImageURL
	// and the content being the image URL

	var msgType MessageType
	var msgContent string
	if message.MultiContent != nil {
		for _, content := range message.MultiContent {
			if content.Type == "image_url" {
				msgType = ImageUrl
				msgContent = string(content.ImageURL.URL)
			}
		}
	} else {
		msgType = Text
		msgContent = message.Content
	}

	id := uuid.New().String()

	return SentinelMessage{
		Id:        &id,
		Role:      SentinelMessageRole(message.Role),
		ToolCalls: &toolCalls,
		Type:      &msgType,
		Content:   msgContent,
	}, nil
}

func convertToolCalls(ctx context.Context, toolCalls []openai.ToolCall, runId uuid.UUID, store ToolStore) ([]SentinelToolCall, error) {
	var result []SentinelToolCall
	for _, toolCall := range toolCalls {
		toolCall, err := convertToolCall(ctx, toolCall, runId, store)
		if err != nil {
			return nil, fmt.Errorf("error converting tool call: %w", err)
		}
		if toolCall != nil {
			result = append(result, *toolCall)
		}
	}
	return result, nil
}

func convertToolCall(ctx context.Context, toolCall openai.ToolCall, runId uuid.UUID, store ToolStore) (*SentinelToolCall, error) {
	// Get this from the DB
	tool, err := store.GetToolFromNameAndRunId(ctx, toolCall.Function.Name, runId)
	if err != nil {
		return nil, fmt.Errorf("error getting tool: %w", err)
	}
	if tool == nil {
		return nil, fmt.Errorf("tool not found: %s", toolCall.Function.Name)
	}

	id := uuid.New().String()

	return &SentinelToolCall{
		Id:        id,
		ToolId:    tool.Id.String(),
		Name:      &toolCall.Function.Name,
		Arguments: &toolCall.Function.Arguments,
	}, nil
}

func extractChatIds(chatId uuid.UUID, choices []SentinelChoice) ChatIds {
	result := ChatIds{
		ChatId:    chatId,
		ChoiceIds: make([]ChoiceIds, 0, len(choices)),
	}

	for _, choice := range choices {
		choiceIds := ChoiceIds{
			ChoiceId:    choice.SentinelId,
			MessageId:   *choice.Message.Id,
			ToolCallIds: make([]ToolCallIds, 0),
		}

		if choice.Message.ToolCalls != nil {
			for _, toolCall := range *choice.Message.ToolCalls {
				choiceIds.ToolCallIds = append(choiceIds.ToolCallIds, ToolCallIds{
					ToolCallId: &toolCall.Id,
					ToolId:     &toolCall.ToolId,
				})
			}
		}

		result.ChoiceIds = append(result.ChoiceIds, choiceIds)
	}

	return result
}

// GetRunMessagesHandler gets the messages for a run
func apiGetRunMessagesHandler(w http.ResponseWriter, r *http.Request, runId uuid.UUID, store Store) {
	ctx := r.Context()

	messages, err := store.GetMessagesForRun(ctx, runId)
	if err != nil {
		sendErrorResponse(w, http.StatusInternalServerError, "error getting messages for run", err.Error())
		return
	}

	respondJSON(w, messages, http.StatusOK)
}

func apiGetToolCallStateHandler(w http.ResponseWriter, r *http.Request, toolCallId uuid.UUID, store Store) {
	ctx := r.Context()

	// First verify the run exists
	toolCall, err := store.GetToolCall(ctx, toolCallId)
	if err != nil {
		sendErrorResponse(w, http.StatusInternalServerError, "error getting tool call", err.Error())
		return
	}
	if toolCall == nil {
		sendErrorResponse(w, http.StatusNotFound, "Run not found", "")
		return
	}

	execution := RunExecution{
		Chains:   make([]ChainExecutionState, 0),
		Toolcall: *toolCall,
	}

	toolId, err := uuid.Parse(toolCall.ToolId)
	if err != nil {
		sendErrorResponse(w, http.StatusInternalServerError, "error parsing tool id", err.Error())
		return
	}

	// Get all chains for this tool
	chains, err := store.GetSupervisorChains(ctx, toolId)
	if err != nil {
		sendErrorResponse(w, http.StatusInternalServerError, "error getting chains", err.Error())
		return
	}

	for _, chain := range chains {
		// Get the chain execution from the chain ID + tool call ID
		chainExecutionId, err := store.GetChainExecutionFromChainAndToolCall(ctx, chain.ChainId, toolCallId)
		if err != nil {
			sendErrorResponse(w, http.StatusInternalServerError, "error getting chain execution", err.Error())
			return
		}

		ceState, err := store.GetChainExecutionState(ctx, *chainExecutionId)
		if err != nil {
			sendErrorResponse(w, http.StatusInternalServerError, "error getting chain execution state", err.Error())
			return
		}

		execution.Chains = append(execution.Chains, *ceState)
	}

	status, err := getToolCallStatus(ctx, toolCallId, store)
	if err != nil {
		sendErrorResponse(w, http.StatusInternalServerError, "error getting tool call status", err.Error())
		return
	}

	execution.Status = status

	respondJSON(w, execution, http.StatusOK)
}
