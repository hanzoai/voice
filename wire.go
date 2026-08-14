// Package voice serves a spoken conversation over one socket.
//
// The wire is the OpenAI realtime protocol, so any client already written
// against that protocol drives this surface unchanged. The event structs come
// from github.com/hanzoai/go-openai-realtime, which is a client: it marshals
// what a client sends and unmarshals what a server sends. Serving the other
// side needs the mirror of that, and only the receiving half is missing —
// hence Read below and nothing else.
package voice

import (
	"encoding/json"
	"fmt"

	rt "github.com/hanzoai/go-openai-realtime"
)

// Frame is a video keyframe offered alongside speech.
//
// The realtime protocol carries audio and no picture. A model that can see
// needs one, so the buffer that holds pictures is named for the buffer that
// holds sound and behaves the same way: append, and the next reply reads it.
const Frame rt.ClientEventType = "input_video_buffer.append"

// FrameEvent appends one still to the video buffer. Bytes are base64 JPEG,
// exactly as ClientEventTypeInputAudioBufferAppend carries base64 PCM.
type FrameEvent struct {
	rt.EventBase
	Image string `json:"image"`
}

func (FrameEvent) ClientEventType() rt.ClientEventType { return Frame }

// Read turns one JSON message from a client into the event it names.
//
// The library ships UnmarshalServerEvent because a client receives server
// events; a server receives client events, and that direction has no reader.
// The type field is the discriminator in both directions, so this is the same
// dispatch pointed the other way.
func Read(data []byte) (rt.ClientEvent, error) {
	var head struct {
		Type rt.ClientEventType `json:"type"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("event is not json: %w", err)
	}
	var out rt.ClientEvent
	switch head.Type {
	case rt.ClientEventTypeSessionUpdate:
		out = &rt.SessionUpdateEvent{}
	case rt.ClientEventTypeInputAudioBufferAppend:
		out = &rt.InputAudioBufferAppendEvent{}
	case rt.ClientEventTypeInputAudioBufferCommit:
		out = &rt.InputAudioBufferCommitEvent{}
	case rt.ClientEventTypeInputAudioBufferClear:
		out = &rt.InputAudioBufferClearEvent{}
	case rt.ClientEventTypeConversationItemCreate:
		out = &rt.ConversationItemCreateEvent{}
	case rt.ClientEventTypeConversationItemTruncate:
		out = &rt.ConversationItemTruncateEvent{}
	case rt.ClientEventTypeConversationItemDelete:
		out = &rt.ConversationItemDeleteEvent{}
	case rt.ClientEventTypeResponseCreate:
		out = &rt.ResponseCreateEvent{}
	case rt.ClientEventTypeResponseCancel:
		out = &rt.ResponseCancelEvent{}
	case Frame:
		out = &FrameEvent{}
	default:
		return nil, fmt.Errorf("unknown event type %q", head.Type)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return nil, fmt.Errorf("event %q is malformed: %w", head.Type, err)
	}
	return out, nil
}
