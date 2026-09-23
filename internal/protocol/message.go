package protocol

import "net/http"

const Version = "0.1.0"

// Body describes a raw body file adjacent to a message metadata file.
type Body struct {
	File string `json:"file"`
	Size int64  `json:"size"`
}

type Request struct {
	ID        string      `json:"id"`
	ClientID  string      `json:"client_id"`
	Service   string      `json:"service"`
	Method    string      `json:"method"`
	Path      string      `json:"path"` // escaped path, without query string
	Query     string      `json:"query,omitempty"`
	Headers   http.Header `json:"headers,omitempty"`
	Body      Body        `json:"body"`
	CreatedAt string      `json:"created_at"`
	Deadline  string      `json:"deadline,omitempty"`
}

type BridgeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Response struct {
	ID        string       `json:"id"`
	ReplyTo   string       `json:"reply_to"`
	Status    int          `json:"status,omitempty"`
	Headers   http.Header  `json:"headers,omitempty"`
	Body      Body         `json:"body"`
	CreatedAt string       `json:"created_at"`
	Error     *BridgeError `json:"error,omitempty"`
}

type RegistryService struct {
	Description string `json:"description,omitempty"`
}

type Registry struct {
	Version   int                        `json:"version"`
	UpdatedAt string                     `json:"updated_at"`
	Services  map[string]RegistryService `json:"services"`
}

type Heartbeat struct {
	UpdatedAt string `json:"updated_at"`
	Version   string `json:"version"`
}
