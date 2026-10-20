package loomy

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"encoding/base64"
	"strings"

	"github.com/google/uuid"
)

// Account service signing.
//
// Every iFlytek CAccount call carries an `Authorization: account {ak}:{sig}`
// header. The signature is HMAC-SHA1 over a nine-segment canonical string
// joined by newlines:
//
//	{METHOD}\n{ESCAPED_PATH}\n{ESCAPED_QUERY}\n{Content-MD5}\n
//	{Content-Type}\n{Date}\n{Nonce}\n{SignedHeaders}\n{CanonicalizedHeaders}
//
// This provider sends no `x-*` headers, so the last two segments are empty and
// the canonical string always ends with two newlines. Keep them: trimming the
// trailing newline changes the signature.
//
// The body must be serialized exactly once and shared between the signature and
// the request. Re-serializing it (for example by passing a struct to the HTTP
// client) can reorder keys or change spacing and invalidate the signature.

// ContentMD5 returns the base64 MD5 of a non-empty body, or "" for an empty one
// (the client does not hash an empty body; it omits both the value and the
// header).
func ContentMD5(body string) string {
	if body == "" {
		return ""
	}
	sum := md5.Sum([]byte(body))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// EscapedPath mirrors the client's canonical path: ensure a leading slash, drop
// a single trailing slash when the path is longer than "/", then escape each
// segment separately so the separating slashes stay literal.
func EscapedPath(rawPath string) string {
	clean := rawPath
	if !strings.HasPrefix(clean, "/") {
		clean = "/" + clean
	}
	if len(clean) > 1 && strings.HasSuffix(clean, "/") {
		clean = clean[:len(clean)-1]
	}
	segments := strings.Split(clean, "/")
	escaped := make([]string, len(segments))
	for i, segment := range segments {
		if segment == "" {
			escaped[i] = ""
			continue
		}
		escaped[i] = EscapeRFC3986(segment)
	}
	return strings.Join(escaped, "/")
}

// EscapedQuery mirrors the client's canonical query: insertion order is
// preserved (the client does not sort), keys and values are escaped, and nil
// values become an empty string.
func EscapedQuery(params []QueryParam) string {
	if len(params) == 0 {
		return ""
	}
	parts := make([]string, 0, len(params))
	for _, param := range params {
		parts = append(parts, EscapeRFC3986(param.Key)+"="+EscapeRFC3986(param.Value))
	}
	return strings.Join(parts, "&")
}

// QueryParam is one canonical query entry. A slice, not a map, because the
// client signs the caller's insertion order.
type QueryParam struct {
	Key   string
	Value string
}

// SignOptions carries the canonical inputs. Date and Nonce are inputs so the
// signing string is a pure function and can be pinned by a unit test.
type SignOptions struct {
	AccessKeyID     string
	AccessKeySecret string
	Method          string
	Path            string
	Query           []QueryParam
	Body            string
	ContentType     string
	Date            string
	Nonce           string
}

// SigningString builds the nine-segment canonical string.
func SigningString(options SignOptions) string {
	contentType := options.ContentType
	if contentType == "" {
		contentType = "application/json"
	}
	return strings.Join([]string{
		strings.ToUpper(options.Method),
		EscapedPath(options.Path),
		EscapedQuery(options.Query),
		ContentMD5(options.Body),
		contentType,
		options.Date,
		options.Nonce,
		"",
		"",
	}, "\n")
}

// Sign returns the base64 HMAC-SHA1 signature.
func Sign(options SignOptions) string {
	mac := hmac.New(sha1.New, []byte(options.AccessKeySecret))
	mac.Write([]byte(SigningString(options)))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// AuthHeaders builds the complete account request header set.
//
// The caller must send `body` verbatim; see the package comment.
func AuthHeaders(accessKeyID, accessKeySecret, method, path, body, date, nonce string) map[string]string {
	signature := Sign(SignOptions{
		AccessKeyID:     accessKeyID,
		AccessKeySecret: accessKeySecret,
		Method:          method,
		Path:            path,
		Body:            body,
		ContentType:     "application/json",
		Date:            date,
		Nonce:           nonce,
	})
	headers := map[string]string{
		"Authorization": "account " + accessKeyID + ":" + signature,
		"Date":          date,
		"Nonce":         nonce,
		"Content-Type":  "application/json",
	}
	if md5Value := ContentMD5(body); md5Value != "" {
		headers["Content-MD5"] = md5Value
	}
	return headers
}

// NewNonce returns a 32-character hex nonce without dashes, matching the
// client's `randomUUID().replace(/-/g, "")` trace id.
func NewNonce() string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")
}
