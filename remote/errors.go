package remote

import (
	"fmt"
	"net/http"

	"emperror.dev/errors"
)

type RequestErrors struct {
	Errors []RequestError `json:"errors"`
}

type RequestError struct {
	response *http.Response
	Code     string `json:"code"`
	Status   string `json:"status"`
	Detail   string `json:"detail"`
}

// IsRequestError checks if the given error is of the RequestError type.
func IsRequestError(err error) bool {
	var rerr *RequestError
	if err == nil {
		return false
	}
	return errors.As(err, &rerr)
}

// AsRequestError transforms the error into a RequestError if it is currently
// one, checking the wrap status from the other error handlers. If the error
// is not a RequestError nil is returned.
func AsRequestError(err error) *RequestError {
	if err == nil {
		return nil
	}
	var rerr *RequestError
	if errors.As(err, &rerr) {
		return rerr
	}
	return nil
}

// Error returns the error response in a string form that can be more easily
// consumed.
func (re *RequestError) Error() string {
	c := 0
	if re.response != nil {
		c = re.response.StatusCode
	}

	return fmt.Sprintf("Error response from Panel: %s: %s (HTTP/%d)", re.Code, re.Detail, c)
}

// StatusCode returns the status code of the response.
func (re *RequestError) StatusCode() int {
	return re.response.StatusCode
}

type SftpInvalidCredentialsError struct{}

func (ice SftpInvalidCredentialsError) Error() string {
	return "the credentials provided were invalid"
}

var (
	// ErrNoWindowsProfile indicates the Panel has no Windows profile for a
	// server's egg. The server cannot be run: a Linux egg's install script and
	// startup command will not work here, and guessing at them would produce a
	// server that appears to install and then fails obscurely.
	ErrNoWindowsProfile = errors.Sentinel("remote: no windows profile is configured for this egg")

	// ErrNoWindowsProfileAPI indicates the Panel is not serving the Windows
	// profile API at all, meaning the win-wings Blueprint plugin is missing or
	// disabled.
	ErrNoWindowsProfileAPI = errors.Sentinel("remote: the panel is not serving the windows profile API")
)
