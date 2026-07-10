package webrtc

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	pion "github.com/pion/webrtc/v4"
	"github.com/tarik02/webdesktop/media"
)

const (
	signalingVersion = 1
	controlVersion   = 1

	signalTypeOffer        = "offer"
	signalTypeAnswer       = "answer"
	signalTypeICECandidate = "ice-candidate"
	signalTypeError        = "error"

	controlTypeQualitySet       = "video.quality.set"
	controlTypeQualitySetResult = "video.quality.set.result"
	controlTypeError            = "error"
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

type signalRequest struct {
	Version   optionalInt       `json:"version"`
	Type      optionalString    `json:"type"`
	SDP       optionalString    `json:"sdp"`
	Candidate optionalCandidate `json:"candidate"`
}

type signalResponse struct {
	Version   int                    `json:"version"`
	Type      string                 `json:"type"`
	SDP       string                 `json:"sdp,omitempty"`
	Candidate *pion.ICECandidateInit `json:"candidate,omitempty"`
	Error     *protocolError         `json:"error,omitempty"`
}

type qualityPatch struct {
	Codec       optionalString `json:"codec"`
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
	Codec       string `json:"codec"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Framerate   int    `json:"framerate"`
	BitrateKbps int    `json:"bitrate_kbps"`
}

type controlResponse struct {
	Version int             `json:"version"`
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	OK      bool            `json:"ok"`
	Quality *controlQuality `json:"quality,omitempty"`
	Error   *protocolError  `json:"error,omitempty"`
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

func qualityResponse(quality media.Quality) *controlQuality {
	return &controlQuality{
		Codec:       quality.Codec,
		Width:       quality.Width,
		Height:      quality.Height,
		Framerate:   quality.Framerate,
		BitrateKbps: quality.BitrateKbps,
	}
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
	if request.Type.Value != controlTypeQualitySet {
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
	if request.Quality.Value.Codec.Set {
		return &protocolError{
			Code:    "codec_static",
			Message: "codec cannot change on an active WebRTC stream",
		}
	}
	if !request.Quality.Value.Width.Set &&
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

func validateH264Offer(raw string) error {
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
		h264Payloads := make(map[string]struct{})
		for _, attribute := range mediaDescription.Attributes {
			if attribute.Key != "rtpmap" {
				continue
			}
			fields := strings.Fields(attribute.Value)
			if len(fields) == 2 && strings.EqualFold(strings.Split(fields[1], "/")[0], "H264") {
				h264Payloads[fields[0]] = struct{}{}
			}
		}
		for _, attribute := range mediaDescription.Attributes {
			if attribute.Key != "fmtp" {
				continue
			}
			fields := strings.SplitN(attribute.Value, " ", 2)
			if len(fields) != 2 {
				continue
			}
			if _, ok := h264Payloads[fields[0]]; !ok {
				continue
			}

			parameters := make(map[string]string)
			for _, parameter := range strings.Split(fields[1], ";") {
				key, value, ok := strings.Cut(parameter, "=")
				if ok {
					parameters[strings.ToLower(strings.TrimSpace(key))] = strings.ToLower(strings.TrimSpace(value))
				}
			}
			if parameters["packetization-mode"] != "1" {
				continue
			}
			profileLevelID, err := hex.DecodeString(parameters["profile-level-id"])
			if err != nil || len(profileLevelID) != 3 {
				continue
			}
			if profileLevelID[0] == 0x42 &&
				profileLevelID[1] == 0xe0 &&
				(profileLevelID[2] >= 0x28 || parameters["level-asymmetry-allowed"] == "1") {
				return nil
			}
		}
	}
	return errors.New("offer does not support browser-compatible H.264 constrained-baseline with packetization mode 1 and Level 4.0 or level asymmetry")
}

func rewriteH264Answer(raw string) (string, error) {
	description := pion.SessionDescription{
		Type: pion.SDPTypeAnswer,
		SDP:  raw,
	}
	parsed, err := description.Unmarshal()
	if err != nil {
		return "", err
	}

	rewritten := false
	for _, mediaDescription := range parsed.MediaDescriptions {
		if mediaDescription.MediaName.Media != "video" {
			continue
		}
		h264Payloads := make(map[string]struct{})
		for _, attribute := range mediaDescription.Attributes {
			if attribute.Key != "rtpmap" {
				continue
			}
			fields := strings.Fields(attribute.Value)
			if len(fields) == 2 && strings.EqualFold(strings.Split(fields[1], "/")[0], "H264") {
				h264Payloads[fields[0]] = struct{}{}
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
			if _, ok := h264Payloads[fields[0]]; !ok {
				continue
			}

			parameters := strings.Split(fields[1], ";")
			for parameterIndex, parameter := range parameters {
				key, _, ok := strings.Cut(parameter, "=")
				if !ok || !strings.EqualFold(strings.TrimSpace(key), "profile-level-id") {
					continue
				}
				parameters[parameterIndex] = "profile-level-id=" + media.H264SDPProfileLevelID
				attribute.Value = fields[0] + " " + strings.Join(parameters, ";")
				rewritten = true
				break
			}
		}
	}
	if !rewritten {
		return "", errors.New("H.264 answer did not contain profile-level-id")
	}

	data, err := parsed.Marshal()
	if err != nil {
		return "", err
	}
	return string(data), nil
}
