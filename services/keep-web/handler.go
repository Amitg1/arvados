package main

import (
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"git.curoverse.com/arvados.git/sdk/go/arvadosclient"
	"git.curoverse.com/arvados.git/sdk/go/auth"
	"git.curoverse.com/arvados.git/sdk/go/httpserver"
	"git.curoverse.com/arvados.git/sdk/go/keepclient"
)

type handler struct {
	Config     *Config
	clientPool *arvadosclient.ClientPool
	setupOnce  sync.Once
}

// parseCollectionIDFromDNSName returns a UUID or PDH if s begins with
// a UUID or URL-encoded PDH; otherwise "".
func parseCollectionIDFromDNSName(s string) string {
	// Strip domain.
	if i := strings.IndexRune(s, '.'); i >= 0 {
		s = s[:i]
	}
	// Names like {uuid}--collections.example.com serve the same
	// purpose as {uuid}.collections.example.com but can reduce
	// cost/effort of using [additional] wildcard certificates.
	if i := strings.Index(s, "--"); i >= 0 {
		s = s[:i]
	}
	if arvadosclient.UUIDMatch(s) {
		return s
	}
	if pdh := strings.Replace(s, "-", "+", 1); arvadosclient.PDHMatch(pdh) {
		return pdh
	}
	return ""
}

var urlPDHDecoder = strings.NewReplacer(" ", "+", "-", "+")

// parseCollectionIDFromURL returns a UUID or PDH if s is a UUID or a
// PDH (even if it is a PDH with "+" replaced by " " or "-");
// otherwise "".
func parseCollectionIDFromURL(s string) string {
	if arvadosclient.UUIDMatch(s) {
		return s
	}
	if pdh := urlPDHDecoder.Replace(s); arvadosclient.PDHMatch(pdh) {
		return pdh
	}
	return ""
}

func (h *handler) setup() {
	h.clientPool = arvadosclient.MakeClientPool()
}

// ServeHTTP implements http.Handler.
func (h *handler) ServeHTTP(wOrig http.ResponseWriter, r *http.Request) {
	h.setupOnce.Do(h.setup)

	var statusCode = 0
	var statusText string

	remoteAddr := r.RemoteAddr
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		remoteAddr = xff + "," + remoteAddr
	}

	w := httpserver.WrapResponseWriter(wOrig)
	defer func() {
		if statusCode == 0 {
			statusCode = w.WroteStatus()
		} else if w.WroteStatus() == 0 {
			w.WriteHeader(statusCode)
		} else if w.WroteStatus() != statusCode {
			httpserver.Log(r.RemoteAddr, "WARNING",
				fmt.Sprintf("Our status changed from %d to %d after we sent headers", w.WroteStatus(), statusCode))
		}
		if statusText == "" {
			statusText = http.StatusText(statusCode)
		}
		httpserver.Log(remoteAddr, statusCode, statusText, w.WroteBodyBytes(), r.Method, r.Host, r.URL.Path, r.URL.RawQuery)
	}()

	if r.Method == "OPTIONS" {
		method := r.Header.Get("Access-Control-Request-Method")
		if method != "GET" && method != "POST" {
			statusCode = http.StatusMethodNotAllowed
			return
		}
		w.Header().Set("Access-Control-Allow-Headers", "Range")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Max-Age", "86400")
		statusCode = http.StatusOK
		return
	}

	if r.Method != "GET" && r.Method != "POST" {
		statusCode, statusText = http.StatusMethodNotAllowed, r.Method
		return
	}

	if r.Header.Get("Origin") != "" {
		// Allow simple cross-origin requests without user
		// credentials ("user credentials" as defined by CORS,
		// i.e., cookies, HTTP authentication, and client-side
		// SSL certificates. See
		// http://www.w3.org/TR/cors/#user-credentials).
		w.Header().Set("Access-Control-Allow-Origin", "*")
	}

	arv := h.clientPool.Get()
	if arv == nil {
		statusCode, statusText = http.StatusInternalServerError, "Pool failed: "+h.clientPool.Err().Error()
		return
	}
	defer h.clientPool.Put(arv)

	pathParts := strings.Split(r.URL.Path[1:], "/")

	var targetID string
	var targetPath []string
	var tokens []string
	var reqTokens []string
	var pathToken bool
	var attachment bool
	credentialsOK := h.Config.TrustAllContent

	if r.Host != "" && r.Host == h.Config.AttachmentOnlyHost {
		credentialsOK = true
		attachment = true
	} else if r.FormValue("disposition") == "attachment" {
		attachment = true
	}

	if targetID = parseCollectionIDFromDNSName(r.Host); targetID != "" {
		// http://ID.collections.example/PATH...
		credentialsOK = true
		targetPath = pathParts
	} else if len(pathParts) >= 2 && strings.HasPrefix(pathParts[0], "c=") {
		// /c=ID/PATH...
		targetID = parseCollectionIDFromURL(pathParts[0][2:])
		targetPath = pathParts[1:]
	} else if len(pathParts) >= 3 && pathParts[0] == "collections" {
		if len(pathParts) >= 5 && pathParts[1] == "download" {
			// /collections/download/ID/TOKEN/PATH...
			targetID = parseCollectionIDFromURL(pathParts[2])
			tokens = []string{pathParts[3]}
			targetPath = pathParts[4:]
			pathToken = true
		} else {
			// /collections/ID/PATH...
			targetID = parseCollectionIDFromURL(pathParts[1])
			tokens = h.Config.AnonymousTokens
			targetPath = pathParts[2:]
		}
	}

	if targetID == "" {
		statusCode = http.StatusNotFound
		return
	}

	formToken := r.FormValue("api_token")
	if formToken != "" && r.Header.Get("Origin") != "" && attachment && r.URL.Query().Get("api_token") == "" {
		// The client provided an explicit token in the POST
		// body. The Origin header indicates this *might* be
		// an AJAX request, in which case redirect-with-cookie
		// won't work: we should just serve the content in the
		// POST response. This is safe because:
		//
		// * We're supplying an attachment, not inline
		//   content, so we don't need to convert the POST to
		//   a GET and avoid the "really resubmit form?"
		//   problem.
		//
		// * The token isn't embedded in the URL, so we don't
		//   need to worry about bookmarks and copy/paste.
		tokens = append(tokens, formToken)
	} else if formToken != "" {
		// The client provided an explicit token in the query
		// string, or a form in POST body. We must put the
		// token in an HttpOnly cookie, and redirect to the
		// same URL with the query param redacted and method =
		// GET.

		if !credentialsOK {
			// It is not safe to copy the provided token
			// into a cookie unless the current vhost
			// (origin) serves only a single collection or
			// we are in TrustAllContent mode.
			statusCode = http.StatusBadRequest
			return
		}

		// The HttpOnly flag is necessary to prevent
		// JavaScript code (included in, or loaded by, a page
		// in the collection being served) from employing the
		// user's token beyond reading other files in the same
		// domain, i.e., same collection.
		//
		// The 303 redirect is necessary in the case of a GET
		// request to avoid exposing the token in the Location
		// bar, and in the case of a POST request to avoid
		// raising warnings when the user refreshes the
		// resulting page.

		http.SetCookie(w, &http.Cookie{
			Name:     "arvados_api_token",
			Value:    auth.EncodeTokenCookie([]byte(formToken)),
			Path:     "/",
			HttpOnly: true,
		})

		// Propagate query parameters (except api_token) from
		// the original request.
		redirQuery := r.URL.Query()
		redirQuery.Del("api_token")

		redir := (&url.URL{
			Host:     r.Host,
			Path:     r.URL.Path,
			RawQuery: redirQuery.Encode(),
		}).String()

		w.Header().Add("Location", redir)
		statusCode, statusText = http.StatusSeeOther, redir
		w.WriteHeader(statusCode)
		io.WriteString(w, `<A href="`)
		io.WriteString(w, html.EscapeString(redir))
		io.WriteString(w, `">Continue</A>`)
		return
	}

	if tokens == nil && strings.HasPrefix(targetPath[0], "t=") {
		// http://ID.example/t=TOKEN/PATH...
		// /c=ID/t=TOKEN/PATH...
		//
		// This form must only be used to pass scoped tokens
		// that give permission for a single collection. See
		// FormValue case above.
		tokens = []string{targetPath[0][2:]}
		pathToken = true
		targetPath = targetPath[1:]
	}

	if tokens == nil {
		if credentialsOK {
			reqTokens = auth.NewCredentialsFromHTTPRequest(r).Tokens
		}
		tokens = append(reqTokens, h.Config.AnonymousTokens...)
	}

	if len(targetPath) > 0 && targetPath[0] == "_" {
		// If a collection has a directory called "t=foo" or
		// "_", it can be served at
		// //collections.example/_/t=foo/ or
		// //collections.example/_/_/ respectively:
		// //collections.example/t=foo/ won't work because
		// t=foo will be interpreted as a token "foo".
		targetPath = targetPath[1:]
	}

	tokenResult := make(map[string]int)
	collection := make(map[string]interface{})
	found := false
	for _, arv.ApiToken = range tokens {
		err := arv.Get("collections", targetID, nil, &collection)
		if err == nil {
			// Success
			found = true
			break
		}
		if srvErr, ok := err.(arvadosclient.APIServerError); ok {
			switch srvErr.HttpStatusCode {
			case 404, 401:
				// Token broken or insufficient to
				// retrieve collection
				tokenResult[arv.ApiToken] = srvErr.HttpStatusCode
				continue
			}
		}
		// Something more serious is wrong
		statusCode, statusText = http.StatusInternalServerError, err.Error()
		return
	}
	if !found {
		if pathToken || !credentialsOK {
			// Either the URL is a "secret sharing link"
			// that didn't work out (and asking the client
			// for additional credentials would just be
			// confusing), or we don't even accept
			// credentials at this path.
			statusCode = http.StatusNotFound
			return
		}
		for _, t := range reqTokens {
			if tokenResult[t] == 404 {
				// The client provided valid token(s), but the
				// collection was not found.
				statusCode = http.StatusNotFound
				return
			}
		}
		// The client's token was invalid (e.g., expired), or
		// the client didn't even provide one.  Propagate the
		// 401 to encourage the client to use a [different]
		// token.
		//
		// TODO(TC): This response would be confusing to
		// someone trying (anonymously) to download public
		// data that has been deleted.  Allow a referrer to
		// provide this context somehow?
		w.Header().Add("WWW-Authenticate", "Basic realm=\"collections\"")
		statusCode = http.StatusUnauthorized
		return
	}

	filename := strings.Join(targetPath, "/")
	kc, err := keepclient.MakeKeepClient(arv)
	if err != nil {
		statusCode, statusText = http.StatusInternalServerError, err.Error()
		return
	}
	if client, ok := kc.Client.(*http.Client); ok && client.Transport != nil {
		// Workaround for https://dev.arvados.org/issues/9005
		if t, ok := client.Transport.(*http.Transport); ok {
			t.DisableKeepAlives = true
		}
	}
	rdr, err := kc.CollectionFileReader(collection, filename)
	if os.IsNotExist(err) {
		statusCode = http.StatusNotFound
		return
	} else if err != nil {
		statusCode, statusText = http.StatusBadGateway, err.Error()
		return
	}
	defer rdr.Close()

	basename := path.Base(filename)
	applyContentDispositionHdr(w, r, basename, attachment)

	modstr, _ := collection["modified_at"].(string)
	modtime, err := time.Parse(time.RFC3339Nano, modstr)
	if err != nil {
		modtime = time.Now()
	}
	http.ServeContent(w, r, basename, modtime, rdr)
}

func applyContentDispositionHdr(w http.ResponseWriter, r *http.Request, filename string, isAttachment bool) {
	disposition := "inline"
	if isAttachment {
		disposition = "attachment"
	}
	if strings.ContainsRune(r.RequestURI, '?') {
		// Help the UA realize that the filename is just
		// "filename.txt", not
		// "filename.txt?disposition=attachment".
		//
		// TODO(TC): Follow advice at RFC 6266 appendix D
		disposition += "; filename=" + strconv.QuoteToASCII(filename)
	}
	if disposition != "inline" {
		w.Header().Set("Content-Disposition", disposition)
	}
}
