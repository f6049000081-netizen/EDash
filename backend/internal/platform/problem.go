package platform

import (
	"encoding/json"
	"net/http"
)

// Problem is an RFC 9457 problem document with a stable machine code (see Problem in api/openapi.yaml).
type Problem struct {
	Type    string `json:"type"`
	Title   string `json:"title"`
	Status  int    `json:"status"`
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
	Charged *bool  `json:"charged,omitempty"`
}

func (p *Problem) Error() string { return p.Code + ": " + p.Message }

func NewProblem(status int, code, title, message string) *Problem {
	return &Problem{Type: "https://docs.gebeya.one/errors/" + code, Title: title, Status: status, Code: code, Message: message}
}

// NotCharged marks a payment failure where no money left the payer's account.
func (p *Problem) NotCharged() *Problem {
	f := false
	cp := *p
	cp.Charged = &f
	return &cp
}

func WriteProblem(w http.ResponseWriter, p *Problem) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
