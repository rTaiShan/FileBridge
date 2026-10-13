package protocol

import "net/http"

const Version = "0.3.0"

const RegistryVersion = 2

// Body describes a raw body file adjacent to a message metadata file.
type Body struct {
	File string `json:"file"`
	Size int64  `json:"size"`
}

type Request struct {
	ServerID string `json:"server_id"`
	// ServerInstanceID binds a request to the server advertisement from which
	// the client selected it. It prevents a restarted server with a changed
	// configuration from executing a request intended for its prior instance.
	ServerInstanceID string      `json:"server_instance_id"`
	ID               string      `json:"id"`
	ClientID         string      `json:"client_id"`
	Service          string      `json:"service"`
	Method           string      `json:"method"`
	Path             string      `json:"path"` // escaped path, without query string
	Query            string      `json:"query,omitempty"`
	Headers          http.Header `json:"headers,omitempty"`
	Body             Body        `json:"body"`
	CreatedAt        string      `json:"created_at"`
	Deadline         string      `json:"deadline,omitempty"`
}

type BridgeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Response struct {
	ID               string       `json:"id"`
	ReplyTo          string       `json:"reply_to"`
	ServerID         string       `json:"server_id"`
	ServerInstanceID string       `json:"server_instance_id"`
	Status           int          `json:"status,omitempty"`
	Headers          http.Header  `json:"headers,omitempty"`
	Body             Body         `json:"body"`
	CreatedAt        string       `json:"created_at"`
	Error            *BridgeError `json:"error,omitempty"`
}

type RegistryService struct {
	Description string `json:"description,omitempty"`
}

// ServerRegistry is published by one server at its own registry path. There
// is deliberately no shared, multi-writer registry file.
type ServerRegistry struct {
	Version    int                        `json:"version"`
	ServerID   string                     `json:"server_id"`
	InstanceID string                     `json:"instance_id"`
	UpdatedAt  string                     `json:"updated_at"`
	Services   map[string]RegistryService `json:"services"`
}

// Registry is the client-side discovery view returned by /_bridge/services.
type Registry struct {
	Version   int                       `json:"version"`
	UpdatedAt string                    `json:"updated_at"`
	Servers   map[string]ServerRegistry `json:"servers"`
}

type Heartbeat struct {
	ServerID   string `json:"server_id"`
	InstanceID string `json:"instance_id"`
	UpdatedAt  string `json:"updated_at"`
	Version    string `json:"version"`
}
