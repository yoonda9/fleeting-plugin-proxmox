package plugin

import (
	"encoding/json"
	"errors"
	"net"
	"strings"
)

// errorClass categorizes an error returned from a call to the Proxmox API so
// callers can decide whether to keep waiting, retry, or fail immediately.
type errorClass int

const (
	// classTerminal will not resolve itself: retrying or waiting longer won't help.
	classTerminal errorClass = iota

	// classTransient is a failure likely caused by a momentarily overloaded or
	// unreachable Proxmox host; the same call is expected to succeed if retried.
	classTransient

	// classAgentNotReady is go-proxmox's literal "guest agent is not running"
	// response: the VM is up but the QEMU agent hasn't started listening yet.
	classAgentNotReady
)

// agentNotRunningMessage is the literal HTTP status line go-proxmox's own
// VirtualMachine.WaitForAgent matches on (vendor virtual_machine.go): Proxmox
// reports it as the reason phrase of a 500 response, so it never becomes a typed
// error.
const agentNotRunningMessage = "500 QEMU guest agent is not running"

// classifyError is pure: it decides how to treat an error returned from a call to
// the Proxmox API based only on the error value, with no I/O or logging.
//
// Callers must pass the raw, unwrapped error go-proxmox returned. The
// agent-not-running check below matches with strings.Contains and survives
// wrapping, but the 500/501 check matches with strings.HasPrefix and does not -
// wrapping (e.g. fmt.Errorf("...: %w", err)) before calling classifyError silently
// turns a transient 500/501 into classTerminal. Wrap the *result* of classifying
// instead, never the error passed in.
func classifyError(err error) errorClass {
	if err == nil {
		return classTerminal
	}

	if strings.Contains(err.Error(), agentNotRunningMessage) {
		return classAgentNotReady
	}

	// go-proxmox's handleResponse (vendor proxmox.go) returns a bare
	// errors.New(res.Status) for HTTP 500 and 501 only - every other status is
	// either specially handled (400) or unmarshalled as if it had succeeded. Both
	// codes indicate a momentarily overloaded or misbehaving Proxmox host, so this
	// classifies them as transient; check this after the more specific
	// agent-not-running case above, since that is also delivered as a 500.
	if strings.HasPrefix(err.Error(), "500 ") || strings.HasPrefix(err.Error(), "501 ") {
		return classTransient
	}

	// Covers everything the HTTP round trip itself can fail with: connection
	// refused/reset, DNS failures, TLS handshake errors, and any timeout -
	// including http.Client.Timeout and a caller ctx deadline, both of which
	// net/http surfaces as a *url.Error implementing net.Error.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return classTransient
	}

	// An overloaded pveproxy (or a proxy in front of it) can return a non-JSON
	// body - typically an HTML error page - for a status go-proxmox otherwise
	// treats as successful. That surfaces here as a raw json decode error.
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return classTransient
	}

	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return classTransient
	}

	return classTerminal
}
