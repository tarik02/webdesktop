package webrtc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	pion "github.com/pion/webrtc/v4"
	"github.com/tarik02/webdesktop/input"
	"github.com/tarik02/webdesktop/media"
)

const (
	signalingVersion = 1
	controlVersion   = 2
	inputVersion     = 1

	signalTypeOffer        = "offer"
	signalTypeAnswer       = "answer"
	signalTypeICECandidate = "ice-candidate"
	signalTypeClientLog    = "client-log"
	signalTypeError        = "error"

	controlTypeQualitySet         = "video.quality.set"
	controlTypeQualitySetResult   = "video.quality.set.result"
	controlTypeInputAcquire       = "input.acquire"
	controlTypeInputAcquireResult = "input.acquire.result"
	controlTypeInputRelease       = "input.release"
	controlTypeInputReleaseResult = "input.release.result"
	controlTypeError              = "error"

	inputTypePointerAbsolute = "input.pointer.motion.absolute"
	inputTypePointerRelative = "input.pointer.motion.relative"
	inputTypePointerButton   = "input.pointer.button"
	inputTypePointerScroll   = "input.pointer.scroll"
	inputTypeKeyboardKey     = "input.keyboard.key"
	inputTypeError           = "error"

	maxClientLogEventBytes       = 128
	maxClientLogDetails          = 32
	maxClientLogDetailKeyBytes   = 64
	maxClientLogDetailValueBytes = 512
)

type protocolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type optionalString struct {
	Value string
	Set   bool
}

func (value *optionalString) UnmarshalJSON(data []byte) error {
	if bytes.Equal(data, []byte("null")) {
		return errors.New("null is not allowed")
	}
	value.Set = true
	return json.Unmarshal(data, &value.Value)
}

type optionalInt struct {
	Value int
	Set   bool
}

type optionalUint32 struct {
	Value uint32
	Set   bool
}

func (value *optionalUint32) UnmarshalJSON(data []byte) error {
	if bytes.Equal(data, []byte("null")) {
		return errors.New("null is not allowed")
	}
	value.Set = true
	return json.Unmarshal(data, &value.Value)
}

type optionalUint64 struct {
	Value uint64
	Set   bool
}

func (value *optionalUint64) UnmarshalJSON(data []byte) error {
	if bytes.Equal(data, []byte("null")) {
		return errors.New("null is not allowed")
	}
	value.Set = true
	return json.Unmarshal(data, &value.Value)
}

type optionalFloat64 struct {
	Value float64
	Set   bool
}

func (value *optionalFloat64) UnmarshalJSON(data []byte) error {
	if bytes.Equal(data, []byte("null")) {
		return errors.New("null is not allowed")
	}
	value.Set = true
	return json.Unmarshal(data, &value.Value)
}

type optionalBool struct {
	Value bool
	Set   bool
}

func (value *optionalBool) UnmarshalJSON(data []byte) error {
	if bytes.Equal(data, []byte("null")) {
		return errors.New("null is not allowed")
	}
	value.Set = true
	return json.Unmarshal(data, &value.Value)
}

func (value *optionalInt) UnmarshalJSON(data []byte) error {
	if bytes.Equal(data, []byte("null")) {
		return errors.New("null is not allowed")
	}
	value.Set = true
	return json.Unmarshal(data, &value.Value)
}

type optionalCandidate struct {
	Value pion.ICECandidateInit
	Set   bool
}

func (value *optionalCandidate) UnmarshalJSON(data []byte) error {
	if bytes.Equal(data, []byte("null")) {
		return errors.New("null is not allowed")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value.Value); err != nil {
		return err
	}
	var trailing struct{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	value.Set = true
	return nil
}

type optionalStringMap struct {
	Value map[string]string
	Set   bool
}

func (value *optionalStringMap) UnmarshalJSON(data []byte) error {
	if bytes.Equal(data, []byte("null")) {
		return errors.New("null is not allowed")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&value.Value); err != nil {
		return err
	}
	var trailing struct{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	value.Set = true
	return nil
}

type signalRequest struct {
	Version   optionalInt       `json:"version"`
	Type      optionalString    `json:"type"`
	SDP       optionalString    `json:"sdp"`
	Candidate optionalCandidate `json:"candidate"`
	Level     optionalString    `json:"level"`
	Event     optionalString    `json:"event"`
	Details   optionalStringMap `json:"details"`
}

type signalResponse struct {
	Version   int                    `json:"version"`
	Type      string                 `json:"type"`
	SDP       string                 `json:"sdp,omitempty"`
	Candidate *pion.ICECandidateInit `json:"candidate,omitempty"`
	Error     *protocolError         `json:"error,omitempty"`
}

type qualityPatch struct {
	Profile     optionalString `json:"profile"`
	Width       optionalInt    `json:"width"`
	Height      optionalInt    `json:"height"`
	Framerate   optionalInt    `json:"framerate"`
	BitrateKbps optionalInt    `json:"bitrate_kbps"`
}

type optionalQuality struct {
	Value qualityPatch
	Set   bool
}

func (value *optionalQuality) UnmarshalJSON(data []byte) error {
	if bytes.Equal(data, []byte("null")) {
		return errors.New("null is not allowed")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value.Value); err != nil {
		return err
	}
	var trailing struct{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	value.Set = true
	return nil
}

type controlRequest struct {
	Version optionalInt     `json:"version"`
	ID      optionalString  `json:"id"`
	Type    optionalString  `json:"type"`
	Quality optionalQuality `json:"quality"`
}

type controlQuality struct {
	Profile     string `json:"profile"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Framerate   int    `json:"framerate"`
	BitrateKbps int    `json:"bitrate_kbps"`
}

type controlInput struct {
	Pointer  bool `json:"pointer"`
	Keyboard bool `json:"keyboard"`
}

type controlResponse struct {
	Version int             `json:"version"`
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	OK      bool            `json:"ok"`
	Quality *controlQuality `json:"quality,omitempty"`
	Input   *controlInput   `json:"input,omitempty"`
	Error   *protocolError  `json:"error,omitempty"`
}

type inputRequest struct {
	Version        optionalInt     `json:"version"`
	Sequence       optionalUint64  `json:"sequence"`
	Type           optionalString  `json:"type"`
	X              optionalFloat64 `json:"x"`
	Y              optionalFloat64 `json:"y"`
	DX             optionalFloat64 `json:"dx"`
	DY             optionalFloat64 `json:"dy"`
	Button         optionalString  `json:"button"`
	Pressed        optionalBool    `json:"pressed"`
	Horizontal     optionalFloat64 `json:"horizontal"`
	Vertical       optionalFloat64 `json:"vertical"`
	StopHorizontal optionalBool    `json:"stop_horizontal"`
	StopVertical   optionalBool    `json:"stop_vertical"`
	Keycode        optionalUint32  `json:"keycode"`
}

type inputResponse struct {
	Version  int            `json:"version"`
	Sequence *uint64        `json:"sequence,omitempty"`
	Type     string         `json:"type"`
	OK       bool           `json:"ok"`
	Error    *protocolError `json:"error,omitempty"`
}

func decodeSignalRequest(data []byte) (signalRequest, error) {
	var request signalRequest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, err
	}
	var trailing struct{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return request, errors.New("multiple JSON values are not allowed")
		}
		return request, err
	}
	return request, nil
}

func decodeControlRequest(data []byte) (controlRequest, error) {
	var request controlRequest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, err
	}
	var trailing struct{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return request, errors.New("multiple JSON values are not allowed")
		}
		return request, err
	}
	return request, nil
}

func decodeInputRequest(data []byte) (inputRequest, error) {
	var request inputRequest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, err
	}
	var trailing struct{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return request, errors.New("multiple JSON values are not allowed")
		}
		return request, err
	}
	return request, nil
}

func qualityResponse(quality media.Quality) *controlQuality {
	return &controlQuality{
		Profile:     quality.Profile,
		Width:       quality.Width,
		Height:      quality.Height,
		Framerate:   quality.Framerate,
		BitrateKbps: quality.BitrateKbps,
	}
}

func validateClientLogRequest(request signalRequest) *protocolError {
	if request.SDP.Set || request.Candidate.Set {
		return &protocolError{
			Code:    "unexpected_field",
			Message: "client-log does not allow sdp or candidate",
		}
	}
	if !request.Level.Set {
		return &protocolError{Code: "missing_field", Message: "level is required"}
	}
	if !request.Event.Set {
		return &protocolError{Code: "missing_field", Message: "event is required"}
	}
	if !request.Details.Set {
		return &protocolError{Code: "missing_field", Message: "details is required"}
	}
	switch request.Level.Value {
	case "debug", "info", "warn", "error":
	default:
		return &protocolError{
			Code:    "invalid_client_log",
			Message: "level must be debug, info, warn, or error",
		}
	}
	if request.Event.Value == "" ||
		len(request.Event.Value) > maxClientLogEventBytes ||
		strings.IndexFunc(request.Event.Value, unicode.IsControl) >= 0 {
		return &protocolError{
			Code:    "invalid_client_log",
			Message: "event must contain between 1 and 128 bytes without control characters",
		}
	}
	if len(request.Details.Value) > maxClientLogDetails {
		return &protocolError{
			Code:    "invalid_client_log",
			Message: "details must contain at most 32 entries",
		}
	}
	for key, value := range request.Details.Value {
		if key == "" ||
			len(key) > maxClientLogDetailKeyBytes ||
			strings.IndexFunc(key, unicode.IsControl) >= 0 {
			return &protocolError{
				Code:    "invalid_client_log",
				Message: "detail keys must contain between 1 and 64 bytes without control characters",
			}
		}
		if len(value) > maxClientLogDetailValueBytes {
			return &protocolError{
				Code:    "invalid_client_log",
				Message: "detail values must contain at most 512 bytes",
			}
		}
	}
	return nil
}

func hasClientLogFields(request signalRequest) bool {
	return request.Level.Set || request.Event.Set || request.Details.Set
}

func validateControlRequest(request controlRequest) *protocolError {
	if !request.Version.Set {
		return &protocolError{
			Code:    "missing_field",
			Message: "version is required",
		}
	}
	if !request.ID.Set {
		return &protocolError{
			Code:    "missing_field",
			Message: "id is required",
		}
	}
	if !request.Type.Set {
		return &protocolError{
			Code:    "missing_field",
			Message: "type is required",
		}
	}
	if request.Version.Value != controlVersion {
		return &protocolError{
			Code:    "unsupported_version",
			Message: fmt.Sprintf("control protocol version %d is not supported", request.Version.Value),
		}
	}
	if request.ID.Value == "" || len(request.ID.Value) > 128 {
		return &protocolError{
			Code:    "invalid_request_id",
			Message: "id must contain between 1 and 128 bytes",
		}
	}
	switch request.Type.Value {
	case controlTypeInputAcquire, controlTypeInputRelease:
		if request.Quality.Set {
			return &protocolError{
				Code:    "unexpected_field",
				Message: "quality is not allowed for input lease requests",
			}
		}
		return nil
	case controlTypeQualitySet:
	default:
		return &protocolError{
			Code:    "unsupported_type",
			Message: fmt.Sprintf("control message type %q is not supported", request.Type.Value),
		}
	}
	if !request.Quality.Set {
		return &protocolError{
			Code:    "missing_field",
			Message: "quality is required",
		}
	}
	if request.Quality.Value.Profile.Set && request.Quality.Value.Profile.Value == "" {
		return &protocolError{
			Code:    "invalid_quality",
			Message: "quality profile must not be empty",
		}
	}
	if !request.Quality.Value.Profile.Set &&
		!request.Quality.Value.Width.Set &&
		!request.Quality.Value.Height.Set &&
		!request.Quality.Value.Framerate.Set &&
		!request.Quality.Value.BitrateKbps.Set {
		return &protocolError{
			Code:    "invalid_quality",
			Message: "quality must include at least one mutable field",
		}
	}
	return nil
}

func validateInputRequest(request inputRequest) (input.Event, *protocolError) {
	if !request.Version.Set {
		return input.Event{}, &protocolError{Code: "missing_field", Message: "version is required"}
	}
	if !request.Sequence.Set {
		return input.Event{}, &protocolError{Code: "missing_field", Message: "sequence is required"}
	}
	if !request.Type.Set {
		return input.Event{}, &protocolError{Code: "missing_field", Message: "type is required"}
	}
	if request.Version.Value != inputVersion {
		return input.Event{}, &protocolError{
			Code:    "unsupported_version",
			Message: fmt.Sprintf("input protocol version %d is not supported", request.Version.Value),
		}
	}
	if request.Sequence.Value == 0 {
		return input.Event{}, &protocolError{
			Code:    "invalid_sequence",
			Message: "sequence must be greater than zero",
		}
	}

	event := input.Event{Sequence: request.Sequence.Value}
	switch request.Type.Value {
	case inputTypePointerAbsolute:
		if !request.X.Set || !request.Y.Set {
			return input.Event{}, &protocolError{Code: "missing_field", Message: "x and y are required"}
		}
		if hasInputFields(request, "x", "y") {
			return input.Event{}, &protocolError{Code: "unexpected_field", Message: "absolute pointer motion contains unrelated fields"}
		}
		if !finite(request.X.Value) || !finite(request.Y.Value) ||
			request.X.Value < 0 || request.X.Value > 1 ||
			request.Y.Value < 0 || request.Y.Value > 1 {
			return input.Event{}, &protocolError{Code: "invalid_pointer", Message: "x and y must be finite numbers between 0 and 1"}
		}
		event.Type = input.EventPointerAbsolute
		event.X = request.X.Value
		event.Y = request.Y.Value
	case inputTypePointerRelative:
		if !request.DX.Set || !request.DY.Set {
			return input.Event{}, &protocolError{Code: "missing_field", Message: "dx and dy are required"}
		}
		if hasInputFields(request, "dx", "dy") {
			return input.Event{}, &protocolError{Code: "unexpected_field", Message: "relative pointer motion contains unrelated fields"}
		}
		if !finite(request.DX.Value) || !finite(request.DY.Value) {
			return input.Event{}, &protocolError{Code: "invalid_pointer", Message: "dx and dy must be finite numbers"}
		}
		event.Type = input.EventPointerRelative
		event.DX = request.DX.Value
		event.DY = request.DY.Value
	case inputTypePointerButton:
		if !request.Button.Set || !request.Pressed.Set {
			return input.Event{}, &protocolError{Code: "missing_field", Message: "button and pressed are required"}
		}
		if hasInputFields(request, "button", "pressed") {
			return input.Event{}, &protocolError{Code: "unexpected_field", Message: "pointer button contains unrelated fields"}
		}
		buttons := map[string]uint32{
			"primary":   0x110,
			"secondary": 0x111,
			"middle":    0x112,
			"forward":   0x115,
			"back":      0x116,
		}
		code, ok := buttons[request.Button.Value]
		if !ok {
			return input.Event{}, &protocolError{
				Code:    "invalid_button",
				Message: "button must be primary, middle, secondary, back, or forward",
			}
		}
		event.Type = input.EventPointerButton
		event.ButtonCode = code
		event.Pressed = request.Pressed.Value
	case inputTypePointerScroll:
		if !request.Horizontal.Set ||
			!request.Vertical.Set ||
			!request.StopHorizontal.Set ||
			!request.StopVertical.Set {
			return input.Event{}, &protocolError{
				Code:    "missing_field",
				Message: "horizontal, vertical, stop_horizontal, and stop_vertical are required",
			}
		}
		if hasInputFields(request, "horizontal", "vertical", "stop_horizontal", "stop_vertical") {
			return input.Event{}, &protocolError{Code: "unexpected_field", Message: "pointer scroll contains unrelated fields"}
		}
		if !finite(request.Horizontal.Value) || !finite(request.Vertical.Value) {
			return input.Event{}, &protocolError{Code: "invalid_scroll", Message: "horizontal and vertical must be finite numbers"}
		}
		if request.Horizontal.Value == 0 &&
			request.Vertical.Value == 0 &&
			!request.StopHorizontal.Value &&
			!request.StopVertical.Value {
			return input.Event{}, &protocolError{Code: "invalid_scroll", Message: "scroll requires a delta or an axis stop"}
		}
		event.Type = input.EventPointerScroll
		event.Horizontal = request.Horizontal.Value
		event.Vertical = request.Vertical.Value
		event.StopHorizontal = request.StopHorizontal.Value
		event.StopVertical = request.StopVertical.Value
	case inputTypeKeyboardKey:
		if !request.Keycode.Set || !request.Pressed.Set {
			return input.Event{}, &protocolError{Code: "missing_field", Message: "keycode and pressed are required"}
		}
		if hasInputFields(request, "keycode", "pressed") {
			return input.Event{}, &protocolError{Code: "unexpected_field", Message: "keyboard key contains unrelated fields"}
		}
		if request.Keycode.Value < 1 || request.Keycode.Value > 0x2ff {
			return input.Event{}, &protocolError{Code: "invalid_keycode", Message: "keycode must be a Linux evdev code between 1 and 767"}
		}
		event.Type = input.EventKeyboardKey
		event.Keycode = request.Keycode.Value
		event.Pressed = request.Pressed.Value
	default:
		return input.Event{}, &protocolError{
			Code:    "unsupported_type",
			Message: fmt.Sprintf("input message type %q is not supported", request.Type.Value),
		}
	}
	return event, nil
}

func hasInputFields(request inputRequest, allowed ...string) bool {
	fields := map[string]bool{
		"x":               request.X.Set,
		"y":               request.Y.Set,
		"dx":              request.DX.Set,
		"dy":              request.DY.Set,
		"button":          request.Button.Set,
		"pressed":         request.Pressed.Set,
		"horizontal":      request.Horizontal.Set,
		"vertical":        request.Vertical.Set,
		"stop_horizontal": request.StopHorizontal.Set,
		"stop_vertical":   request.StopVertical.Set,
		"keycode":         request.Keycode.Set,
	}
	for _, name := range allowed {
		delete(fields, name)
	}
	for _, set := range fields {
		if set {
			return true
		}
	}
	return false
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func validateAudioOffer(raw string) error {
	description := pion.SessionDescription{
		Type: pion.SDPTypeOffer,
		SDP:  raw,
	}
	parsed, err := description.Unmarshal()
	if err != nil {
		return err
	}

	foundAudio := false
	recvCapableCount := 0
	compatible := false
	for _, mediaDescription := range parsed.MediaDescriptions {
		if mediaDescription.MediaName.Media != "audio" {
			continue
		}
		foundAudio = true
		if mediaDescription.MediaName.Port.Value == 0 {
			continue
		}

		direction := ""
		for _, attribute := range mediaDescription.Attributes {
			switch attribute.Key {
			case "sendrecv", "sendonly", "recvonly", "inactive":
				direction = attribute.Key
			}
		}
		if direction != "recvonly" && direction != "sendrecv" {
			continue
		}
		recvCapableCount++

		formats := make(map[string]struct{}, len(mediaDescription.MediaName.Formats))
		for _, format := range mediaDescription.MediaName.Formats {
			formats[format] = struct{}{}
		}
		for _, attribute := range mediaDescription.Attributes {
			if attribute.Key != "rtpmap" {
				continue
			}
			fields := strings.Fields(attribute.Value)
			if len(fields) != 2 {
				continue
			}
			if _, ok := formats[fields[0]]; !ok {
				continue
			}
			codec := strings.Split(fields[1], "/")
			if len(codec) == 3 &&
				strings.EqualFold(codec[0], "opus") &&
				codec[1] == "48000" &&
				codec[2] == "2" {
				compatible = true
				break
			}
		}
	}

	switch {
	case !foundAudio:
		return errors.New("offer does not contain the audio media section required by enabled audio")
	case recvCapableCount == 0:
		return errors.New("offer does not contain an active recv-capable audio media section")
	case recvCapableCount > 1:
		return errors.New("offer contains multiple active recv-capable audio media sections")
	case compatible:
		return nil
	default:
		return errors.New("offer does not advertise Opus/48000/2 in an active recv-capable audio media section")
	}
}

func validateVideoOffer(raw string, codec media.RTPCodec) error {
	description := pion.SessionDescription{
		Type: pion.SDPTypeOffer,
		SDP:  raw,
	}
	parsed, err := description.Unmarshal()
	if err != nil {
		return err
	}

	for _, mediaDescription := range parsed.MediaDescriptions {
		if mediaDescription.MediaName.Media != "video" {
			continue
		}
		codecPayloads := make(map[string]struct{})
		for _, attribute := range mediaDescription.Attributes {
			if attribute.Key != "rtpmap" {
				continue
			}
			fields := strings.Fields(attribute.Value)
			if len(fields) != 2 {
				continue
			}
			codecFields := strings.Split(fields[1], "/")
			if len(codecFields) >= 2 &&
				strings.EqualFold(codecFields[0], strings.TrimPrefix(codec.MimeType, "video/")) &&
				codecFields[1] == strconv.FormatUint(uint64(codec.ClockRate), 10) &&
				((codec.Channels == 0 && len(codecFields) == 2) ||
					(codec.Channels > 0 && len(codecFields) == 3 && codecFields[2] == strconv.FormatUint(uint64(codec.Channels), 10))) {
				codecPayloads[fields[0]] = struct{}{}
			}
		}
		if len(codec.SDP.OfferFmtp) == 0 && len(codecPayloads) > 0 {
			return nil
		}
		for _, attribute := range mediaDescription.Attributes {
			if attribute.Key != "fmtp" {
				continue
			}
			fields := strings.SplitN(attribute.Value, " ", 2)
			if len(fields) != 2 {
				continue
			}
			if _, ok := codecPayloads[fields[0]]; !ok {
				continue
			}

			parameters := make(map[string]string)
			for _, parameter := range strings.Split(fields[1], ";") {
				key, value, ok := strings.Cut(parameter, "=")
				if ok {
					parameters[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
				}
			}
			compatible := true
			for key, pattern := range codec.SDP.OfferFmtp {
				value, exists := parameters[strings.ToLower(key)]
				if !exists || !regexp.MustCompile(pattern).MatchString(value) {
					compatible = false
					break
				}
			}
			if compatible {
				return nil
			}
		}
	}
	return fmt.Errorf("offer does not support configured video codec %q", codec.ID)
}

func rewriteVideoAnswer(raw string, codec media.RTPCodec) (string, error) {
	description := pion.SessionDescription{
		Type: pion.SDPTypeAnswer,
		SDP:  raw,
	}
	parsed, err := description.Unmarshal()
	if err != nil {
		return "", err
	}

	rewritten := make(map[string]bool, len(codec.SDP.AnswerFmtp))
	for _, mediaDescription := range parsed.MediaDescriptions {
		if mediaDescription.MediaName.Media != "video" {
			continue
		}
		codecPayloads := make(map[string]struct{})
		for _, attribute := range mediaDescription.Attributes {
			if attribute.Key != "rtpmap" {
				continue
			}
			fields := strings.Fields(attribute.Value)
			if len(fields) != 2 {
				continue
			}
			codecFields := strings.Split(fields[1], "/")
			if len(codecFields) >= 2 &&
				strings.EqualFold(codecFields[0], strings.TrimPrefix(codec.MimeType, "video/")) &&
				codecFields[1] == strconv.FormatUint(uint64(codec.ClockRate), 10) &&
				((codec.Channels == 0 && len(codecFields) == 2) ||
					(codec.Channels > 0 && len(codecFields) == 3 && codecFields[2] == strconv.FormatUint(uint64(codec.Channels), 10))) {
				codecPayloads[fields[0]] = struct{}{}
			}
		}
		for index := range mediaDescription.Attributes {
			attribute := &mediaDescription.Attributes[index]
			if attribute.Key != "fmtp" {
				continue
			}
			fields := strings.SplitN(attribute.Value, " ", 2)
			if len(fields) != 2 {
				continue
			}
			if _, ok := codecPayloads[fields[0]]; !ok {
				continue
			}

			parameters := strings.Split(fields[1], ";")
			for key, value := range codec.SDP.AnswerFmtp {
				for parameterIndex, parameter := range parameters {
					parameterKey, _, ok := strings.Cut(parameter, "=")
					if !ok || !strings.EqualFold(strings.TrimSpace(parameterKey), key) {
						continue
					}
					parameters[parameterIndex] = key + "=" + value
					attribute.Value = fields[0] + " " + strings.Join(parameters, ";")
					rewritten[key] = true
					break
				}
			}
		}
	}
	if len(rewritten) != len(codec.SDP.AnswerFmtp) {
		return "", fmt.Errorf("video answer did not contain configured %s fmtp parameters", codec.ID)
	}

	data, err := parsed.Marshal()
	if err != nil {
		return "", err
	}
	return string(data), nil
}
