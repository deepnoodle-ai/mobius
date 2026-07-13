package mobius

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/deepnoodle-ai/mobius/mobius/api"
	"github.com/deepnoodle-ai/wonton/assert"
)

const transcriptContractDir = "../internal/testdata/contract"

type transcriptFrameContract struct {
	Events []struct {
		ID    string          `json:"id"`
		Frame json.RawMessage `json:"frame"`
	} `json:"events"`
	Expected struct {
		Cursor                  string   `json:"cursor"`
		MessageIDs              []string `json:"message_ids"`
		RenderableIDs           []string `json:"renderable_ids"`
		RenderableTurnID        string   `json:"renderable_turn_id"`
		RenderableTurnIDs       []string `json:"renderable_turn_ids"`
		NullContentID           string   `json:"null_content_id"`
		ResolvedActionMessageID string   `json:"resolved_action_message_id"`
		ResolvedActionName      string   `json:"resolved_action_name"`
		ToolShapeMessageID      string   `json:"tool_shape_message_id"`
		FlatResolvedActionName  string   `json:"flat_resolved_action_name"`
		MetaResolvedActionName  string   `json:"meta_resolved_action_name"`
		HelpWireName            string   `json:"help_wire_name"`
		DedupedToolBlockCount   int      `json:"deduped_tool_block_count"`
		ToolResultTextMessageID string   `json:"tool_result_text_message_id"`
		ToolResultText          string   `json:"tool_result_text"`
		FailedTurnID            string   `json:"failed_turn_id"`
		FailedTurnErrorType     string   `json:"failed_turn_error_type"`
		FailedTurnErrorMessage  string   `json:"failed_turn_error_message"`
	} `json:"expected"`
}

func TestTranscriptFrameContract(t *testing.T) {
	var fixture transcriptFrameContract
	readTranscriptFixture(t, "transcript_frames.json", &fixture)
	view := NewSessionTranscript()
	for _, event := range fixture.Events {
		var frame api.SessionTranscriptFrame
		assert.NoError(t, frame.UnmarshalJSON(event.Frame))
		view.Apply(TranscriptStreamEvent{ID: event.ID, Frame: frame})
	}

	assert.Equal(t, view.Cursor(), fixture.Expected.Cursor)
	assert.Equal(t, transcriptMessageIDs(view.Messages()), fixture.Expected.MessageIDs)
	visible := view.RenderableMessages()
	assert.Equal(t, transcriptMessageIDs(visible), fixture.Expected.RenderableIDs)
	assert.Equal(t, transcriptMessageIDs(view.RenderableMessagesForTurn(fixture.Expected.RenderableTurnID)), fixture.Expected.RenderableTurnIDs)
	assert.NotNil(t, mustMessage(t, view, fixture.Expected.NullContentID).Content)

	resolvedMessage := mustMessage(t, view, fixture.Expected.ResolvedActionMessageID)
	tool, err := resolvedMessage.Content[0].AsSessionToolUseBlock()
	assert.NoError(t, err)
	assert.Equal(t, NormalizeToolUse(tool).ResolvedAction.Name, fixture.Expected.ResolvedActionName)
	shapeMessage := mustMessage(t, view, fixture.Expected.ToolShapeMessageID)
	flat, err := shapeMessage.Content[0].AsSessionToolUseBlock()
	assert.NoError(t, err)
	meta, err := shapeMessage.Content[1].AsSessionToolUseBlock()
	assert.NoError(t, err)
	help, err := shapeMessage.Content[2].AsSessionToolUseBlock()
	assert.NoError(t, err)
	assert.Equal(t, NormalizeToolUse(flat).ResolvedAction.Name, fixture.Expected.FlatResolvedActionName)
	assert.Equal(t, NormalizeToolUse(meta).ResolvedAction.Name, fixture.Expected.MetaResolvedActionName)
	normalizedHelp := NormalizeToolUse(help)
	assert.Equal(t, normalizedHelp.WireName, fixture.Expected.HelpWireName)
	assert.Nil(t, normalizedHelp.ResolvedAction)

	var final *api.SessionTranscriptMessage
	for _, message := range visible {
		if message.Id == "m_final" {
			final = message
		}
	}
	assert.NotNil(t, final)
	assert.Equal(t, len(final.Content), fixture.Expected.DedupedToolBlockCount)

	resultMessage := mustMessage(t, view, fixture.Expected.ToolResultTextMessageID)
	result, err := resultMessage.Content[0].AsSessionToolResultBlock()
	assert.NoError(t, err)
	assert.Equal(t, ToolResultText(result), fixture.Expected.ToolResultText)

	failed, ok := view.Turn(fixture.Expected.FailedTurnID)
	assert.True(t, ok)
	assert.Equal(t, *failed.ErrorType, fixture.Expected.FailedTurnErrorType)
	assert.Equal(t, *failed.ErrorMessage, fixture.Expected.FailedTurnErrorMessage)
}

type transcriptSnapshotContract struct {
	Pages    []json.RawMessage `json:"pages"`
	Expected struct {
		Cursor        string   `json:"cursor"`
		MessageIDs    []string `json:"message_ids"`
		RenderableIDs []string `json:"renderable_ids"`
		TurnID        string   `json:"turn_id"`
		TurnStatus    string   `json:"turn_status"`
	} `json:"expected"`
}

func TestTranscriptSnapshotContract(t *testing.T) {
	var fixture transcriptSnapshotContract
	readTranscriptFixture(t, "transcript_snapshot.json", &fixture)
	view := NewSessionTranscript()
	for _, raw := range fixture.Pages {
		var page api.SessionTranscriptSnapshot
		assert.NoError(t, json.Unmarshal(raw, &page))
		view.ApplySnapshot(&page)
	}
	assert.Equal(t, view.Cursor(), fixture.Expected.Cursor)
	assert.Equal(t, transcriptMessageIDs(view.Messages()), fixture.Expected.MessageIDs)
	assert.Equal(t, transcriptMessageIDs(view.RenderableMessages()), fixture.Expected.RenderableIDs)
	turn, ok := view.Turn(fixture.Expected.TurnID)
	assert.True(t, ok)
	assert.Equal(t, turn.Status, fixture.Expected.TurnStatus)
}

func readTranscriptFixture(t *testing.T, name string, target interface{}) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(transcriptContractDir, name))
	assert.NoError(t, err)
	assert.NoError(t, json.Unmarshal(raw, target))
}

func transcriptMessageIDs(messages []*api.SessionTranscriptMessage) []string {
	ids := make([]string, len(messages))
	for i, message := range messages {
		ids[i] = message.Id
	}
	return ids
}
