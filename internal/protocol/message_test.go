package protocol

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestRequestAndResponseJSONRoundTrip(t *testing.T) {
	request := Request{
		ID:       "abc123",
		ClientID: "alice-laptop",
		Service:  "reports",
		Method:   http.MethodPost,
		Path:     "/status?mode=full",
		Query:    "mode=full",
		Headers: http.Header{
			"X-Test": []string{"value"},
		},
		Body:      Body{File: "abc123.request.body", Size: 7},
		CreatedAt: "2026-09-23T00:00:00Z",
	}

	b, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("json.Marshal(Request) error = %v", err)
	}
	var decoded Request
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("json.Unmarshal(Request) error = %v", err)
	}
	if decoded.Service != "reports" || decoded.Headers.Get("X-Test") != "value" {
		t.Fatalf("decoded request mismatch: %#v", decoded)
	}

	response := Response{
		ID:      "abc123",
		ReplyTo: "abc123",
		Status:  http.StatusCreated,
		Headers: http.Header{
			"X-Remote": []string{"yes"},
		},
		Body:      Body{File: "abc123.response.body", Size: 7},
		CreatedAt: "2026-09-23T00:00:00Z",
		Error:     &BridgeError{Code: "upstream_unavailable", Message: "no reply"},
	}

	b, err = json.Marshal(response)
	if err != nil {
		t.Fatalf("json.Marshal(Response) error = %v", err)
	}
	var decodedResponse Response
	if err := json.Unmarshal(b, &decodedResponse); err != nil {
		t.Fatalf("json.Unmarshal(Response) error = %v", err)
	}
	if decodedResponse.Status != http.StatusCreated || decodedResponse.Error == nil || decodedResponse.Error.Code != "upstream_unavailable" {
		t.Fatalf("decoded response mismatch: %#v", decodedResponse)
	}
}

func TestVersionIsExposed(t *testing.T) {
	if Version == "" {
		t.Fatal("Version should not be empty")
	}
}
