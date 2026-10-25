package api

import (
	"net/url"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// masked stands in for whatever a model service's address carried that
// could be a credential (#718).
const masked = "redacted"

// maskAddress is a model service's address as the node shows it to the
// app, a browser or an agent: a user name and password, the query and a
// fragment are each replaced with "redacted", and the rest is left
// as saved so that the address still says where the service is. An
// address that cannot be read is replaced whole, since what is in it
// cannot be told.
func maskAddress(address string) string {
	u, err := url.Parse(address)
	if err != nil {
		return masked
	}
	if !credentialed(u) {
		return address
	}
	if u.User != nil {
		u.User = url.User(masked)
	}
	// The whole query goes, keys and all: a token can be a key with no
	// value, as in ?sk-secret.
	if u.RawQuery != "" {
		u.RawQuery = masked
	}
	u.ForceQuery = false
	if u.Fragment != "" || u.RawFragment != "" {
		u.Fragment, u.RawFragment = masked, ""
	}
	return u.String()
}

// credentialed reports whether an address has anywhere a credential could
// be put in it: a user name or password, a query or a fragment.
func credentialed(u *url.URL) bool {
	return u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != ""
}

// modelAddress works out which address a request to save or use a model
// service means. One the node already holds, given as it was saved or as
// the node showed it masked, stands for itself, so that renaming or
// switching to a profile keeps its address. A new one must have no
// credential in it: a key goes in the API key field, which the node never
// gives out. This holds for the address of the Ollama a machine lends from
// as for a planner's.
func modelAddress(given string, held ...string) (string, error) {
	for _, address := range held {
		if address != "" && (given == address || given == maskAddress(address)) {
			return address, nil
		}
	}
	// One that cannot be read is left to the checks of whoever uses it, as
	// it always was: it is shown masked whole.
	if u, err := url.Parse(given); err == nil && credentialed(u) {
		return "", status.Error(codes.InvalidArgument, "a model service address cannot carry a user name, password, query or fragment: give the service's key in the API key field instead")
	}
	return given, nil
}
