package restgw

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/wandering-compiler/sdk/go/lib/principal"
)

// ErrOrgPathConflict: the path names one organization and the W17-Org header
// another. Refused rather than resolved — either answer would act in an org
// the caller did not mean, and which one depends on nothing the caller chose.
var ErrOrgPathConflict = errors.New("the path and the W17-Org header name different organizations")

// WithOrgFromPath makes the `{param}` path segment the request's active
// organization, exactly as if the caller had sent it in the W17-Org header —
// which is what it becomes, BEFORE authentication. Authenticate resolves and
// validates the active org from that header (and narrows grants to it), so
// the path value gets the same membership check a header value always got.
//
// Two consequences worth stating:
//   - An org in the URL is what makes a URL name what it shows: a deep link
//     works, and a client cache keyed by URL tells two orgs apart.
//   - The auth cache never keys on the URL, and does not need to: a request
//     carrying W17-Org bypasses it (see hashHeaders), and this one now does.
//
// A request that also carries W17-Org naming a DIFFERENT org is refused
// (ErrOrgPathConflict); the same org twice is fine.
func WithOrgFromPath(r *http.Request, param string) (*http.Request, error) {
	v := chi.URLParam(r, param)
	if v == "" {
		return nil, fmt.Errorf("path param %q is empty — it names the organization", param)
	}
	if h := r.Header.Get(principal.OrgScopeHeader); h != "" && h != v {
		return nil, fmt.Errorf("%w (path %q, header %q)", ErrOrgPathConflict, v, h)
	}
	cloned := r.Clone(r.Context())
	cloned.Header.Set(principal.OrgScopeHeader, v)
	return cloned, nil
}
