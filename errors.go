package shisade

// Errors this package returns. Callers distinguish them with errors.As, in the
// same places the Python client catches ReadoutError and QuestionError.
// ReadoutHTTPError is a ReadoutError as well, because the Python class it
// replaces subclasses it; QuestionError is separate on both sides.

// ReadoutError reports that the readout could not produce an answer: a request
// failed, the server and the client disagree about the tokenizer, or a code
// could not be resolved.
type ReadoutError struct {
	Message string
	Err     error
}

func (e *ReadoutError) Error() string {
	if e.Err == nil {
		return "shisade: " + e.Message
	}
	return "shisade: " + e.Message + ": " + e.Err.Error()
}

func (e *ReadoutError) Unwrap() error { return e.Err }

// QuestionError reports a question that cannot be rendered, or one outside what
// the readout supports. It is raised before any request is sent.
type QuestionError struct {
	Message string
}

func (e *QuestionError) Error() string { return "shisade: " + e.Message }
