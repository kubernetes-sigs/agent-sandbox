package workloadapi

// SudoRequest initiates a sudo operation
type SudoRequest struct {
	Command []string `json:"command"`
}

// SudoResponse is the streamed response for the progress of a sudo operation
type SudoResponse struct {
	Stream   string `json:"stream,omitempty"`
	Data     string `json:"data,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
}
