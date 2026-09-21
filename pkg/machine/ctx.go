package machine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mayswind/ezbookkeeping/pkg/core"
	"github.com/mayswind/ezbookkeeping/pkg/errs"
	"github.com/mayswind/ezbookkeeping/pkg/models"
	"github.com/mayswind/ezbookkeeping/pkg/settings"
)

// Ctx is the per-request context every machine-plane handler receives
type Ctx struct {
	Gin    *gin.Context
	Web    *core.WebContext
	Config *settings.Config
	Route  *RouteDef

	// Uid and User are the bound user (apis.mdx §6); zero/nil on NoUser routes
	Uid  int64
	User *models.User

	// Loc is the timezone that decides what "a day" is for this call (apis.mdx §17.4)
	Loc *time.Location
	// Client is the advisory X-Ezbk-Client value, for logs only
	Client string

	// WriteTierOn reports whether the server's write tier is on; a DryRunnable write route gets
	// here with it off and must refuse the apply step (RunWrite does)
	WriteTierOn bool

	meta     map[string]any
	body     []byte
	bodyRead bool
}

// SetMeta adds a field to the envelope's meta (truncated, limitApplied, composed, partial, …)
func (mc *Ctx) SetMeta(key string, value any) {
	if mc.meta == nil {
		mc.meta = map[string]any{}
	}

	mc.meta[key] = value
}

// Truncated marks the response as clamped by a cap (R5)
func (mc *Ctx) Truncated(limitApplied int) {
	mc.SetMeta("truncated", true)
	mc.SetMeta("limitApplied", limitApplied)
}

// Param returns a path parameter
func (mc *Ctx) Param(name string) string {
	return mc.Gin.Param(name)
}

// Query returns a query-string argument
func (mc *Ctx) Query(name string) string {
	return strings.TrimSpace(mc.Gin.Query(name))
}

// QueryList returns a list argument given either repeated (`a=1&a=2`) or comma-separated (`a=1,2`)
func (mc *Ctx) QueryList(name string) []string {
	var out []string

	for _, v := range mc.Gin.QueryArray(name) {
		for _, part := range strings.Split(v, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
	}

	// also accept the `name[]` spelling
	for _, v := range mc.Gin.QueryArray(name + "[]") {
		for _, part := range strings.Split(v, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
	}

	return out
}

// QueryInt returns an integer argument, or def when absent
func (mc *Ctx) QueryInt(name string, def int64) (int64, error) {
	v := mc.Query(name)

	if v == "" {
		return def, nil
	}

	n, err := strconv.ParseInt(v, 10, 64)

	if err != nil {
		return 0, Invalid("pass "+name+" as a whole number", "%s must be an integer, got %q", name, v)
	}

	return n, nil
}

// QueryBool returns a boolean argument, or def when absent
func (mc *Ctx) QueryBool(name string, def bool) (bool, error) {
	v := strings.ToLower(mc.Query(name))

	switch v {
	case "":
		return def, nil
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	}

	return false, Invalid("pass "+name+"=true or "+name+"=false", "%s must be a boolean, got %q", name, v)
}

// RawBody returns the request body bytes (read once)
func (mc *Ctx) RawBody() ([]byte, error) {
	if mc.bodyRead {
		return mc.body, nil
	}

	mc.bodyRead = true

	if mc.Gin.Request.Body == nil {
		return nil, nil
	}

	data, err := io.ReadAll(io.LimitReader(mc.Gin.Request.Body, MaxBodyBytes+1))

	if err != nil {
		return nil, Invalid("send a JSON body", "cannot read the request body")
	}

	if len(data) > MaxBodyBytes {
		return nil, Invalid("split the request into smaller batches", "request body exceeds %d bytes", MaxBodyBytes)
	}

	mc.body = data

	return data, nil
}

// BindBody decodes the JSON body into dst, rejecting unknown top-level fields (apis.mdx §7.7). An
// empty body decodes as {}.
func (mc *Ctx) BindBody(dst any) error {
	data, err := mc.RawBody()

	if err != nil {
		return err
	}

	if len(bytes.TrimSpace(data)) == 0 {
		data = []byte("{}")
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	dec.UseNumber()

	if err := dec.Decode(dst); err != nil {
		msg := err.Error()

		if strings.HasPrefix(msg, "json: unknown field ") {
			return Invalid("remove the field, or check its spelling against GET /machine/v1/capabilities", "unknown argument %s", strings.TrimPrefix(msg, "json: unknown field "))
		}

		return Invalid("send a valid JSON object; amounts are integer hundredths, ids are strings", "malformed request body: %s", msg)
	}

	return nil
}

// upstreamEngine is the gin engine Mount() received; contexts for upstream calls are made from it
var upstreamEngine *gin.Engine

// machineTokenId marks the synthetic claims the plane sets; it never exists in token_record
const machineTokenId = "machine-plane"

func (mc *Ctx) newUpstreamContext(method string, query url.Values, body io.Reader, contentType string) (*core.WebContext, error) {
	target := "/api/v1/machine"

	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(mc.Gin.Request.Context(), method, target, body)

	if err != nil {
		return nil, err
	}

	req.RemoteAddr = mc.Gin.Request.RemoteAddr
	req.Host = mc.Gin.Request.Host

	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	if mc.Loc != nil {
		req.Header.Set(core.ClientTimezoneNameHeaderName, mc.Loc.String())
		_, offset := time.Now().In(mc.Loc).Zone()
		req.Header.Set(core.ClientTimezoneOffsetHeaderName, strconv.Itoa(offset/60))
	}

	req.Header.Set(core.AcceptLanguageHeaderName, "en")

	engine := upstreamEngine

	if engine == nil {
		engine = gin.New()
	}

	gc := gin.CreateTestContextOnly(httptest.NewRecorder(), engine)
	gc.Request = req

	web := core.WrapWebContext(gc, mc.Config.TrustedProxyIPs)
	web.SetContextId(mc.Web.GetContextId())

	if mc.User != nil {
		now := time.Now().Unix()
		web.SetTokenClaims(&core.UserTokenClaims{
			UserTokenId: machineTokenId,
			Uid:         mc.Uid,
			Username:    mc.User.Username,
			Type:        core.USER_TOKEN_TYPE_NORMAL,
			IssuedAt:    now,
			ExpiresAt:   now + 300,
		})
	}

	return web, nil
}

// CallUpstream invokes an upstream /api/v1 handler function as the bound user, with a synthetic
// request carrying the given query string and JSON body. This is how the plane reuses upstream's
// validation and services instead of re-implementing them (apis.mdx §2.3, R1).
func (mc *Ctx) CallUpstream(fn core.ApiHandlerFunc, method string, query url.Values, body any) (any, error) {
	var reader io.Reader
	contentType := ""

	if body != nil {
		data, err := json.Marshal(body)

		if err != nil {
			return nil, err
		}

		reader = bytes.NewReader(data)
		contentType = "application/json"
	}

	web, err := mc.newUpstreamContext(method, query, reader, contentType)

	if err != nil {
		return nil, err
	}

	result, uerr := fn(web)

	if uerr != nil {
		return nil, Upstream(uerr)
	}

	return result, nil
}

// CallUpstreamInto is CallUpstream followed by a JSON round-trip of the result into out, so a
// handler can work with a typed view of upstream's response
func (mc *Ctx) CallUpstreamInto(fn core.ApiHandlerFunc, method string, query url.Values, body any, out any) error {
	result, err := mc.CallUpstream(fn, method, query, body)

	if err != nil {
		return err
	}

	data, err := json.Marshal(result)

	if err != nil {
		return err
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	return dec.Decode(out)
}

// CallUpstreamData invokes an upstream data handler (CSV/TSV export)
func (mc *Ctx) CallUpstreamData(fn core.DataHandlerFunc, query url.Values) ([]byte, string, error) {
	web, err := mc.newUpstreamContext(http.MethodGet, query, nil, "")

	if err != nil {
		return nil, "", err
	}

	data, fileName, uerr := fn(web)

	if uerr != nil {
		return nil, "", Upstream(uerr)
	}

	return data, fileName, nil
}

// MultipartFile is one file part for CallUpstreamMultipart
type MultipartFile struct {
	Field    string
	FileName string
	Data     []byte
}

// CallUpstreamMultipart invokes an upstream handler that reads a multipart form (file import parsing)
func (mc *Ctx) CallUpstreamMultipart(fn core.ApiHandlerFunc, fields map[string]string, files []MultipartFile) (any, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			return nil, err
		}
	}

	for _, f := range files {
		part, err := w.CreateFormFile(f.Field, f.FileName)

		if err != nil {
			return nil, err
		}

		if _, err := part.Write(f.Data); err != nil {
			return nil, err
		}
	}

	if err := w.Close(); err != nil {
		return nil, err
	}

	web, err := mc.newUpstreamContext(http.MethodPost, nil, &buf, w.FormDataContentType())

	if err != nil {
		return nil, err
	}

	result, uerr := fn(web)

	if uerr != nil {
		return nil, Upstream(uerr)
	}

	return result, nil
}

// RawResult makes the plane answer with raw bytes (CSV/TSV) instead of the JSON envelope
type RawResult struct {
	ContentType string
	FileName    string
	Data        []byte
}

// RequireWriteTier refuses unless the server's write tier is on. RunWrite calls it for the apply
// step; handlers that do not use RunWrite (undo, redo) call it themselves.
func (mc *Ctx) RequireWriteTier() error {
	if mc.WriteTierOn {
		return nil
	}

	return NewFail(CodeWriteDisabled, "restart the app with writes allowed: ezbk stop && ezbk up --allow-write (sets EZBK_MACHINE_ALLOW_WRITE=1)", "the write tier is off on this server")
}

// ResolveId parses an ezBookkeeping id (a decimal string, apis.mdx §17.5)
func ResolveId(name, v string) (int64, error) {
	v = strings.TrimSpace(v)

	if v == "" {
		return 0, Invalid("pass "+name, "%s is required", name)
	}

	n, err := strconv.ParseInt(v, 10, 64)

	if err != nil || n <= 0 {
		return 0, Invalid("ids are the decimal strings the list routes return", "%s %q is not an ezBookkeeping id", name, v)
	}

	return n, nil
}

// idString renders an id the way the wire carries it
func idString(id int64) string {
	return strconv.FormatInt(id, 10)
}

// ensure errs stays imported for handlers that use UpErr in this file's package
var _ = errs.ErrOperationFailed

// describeUser is the short form of the bound user used in logs and meta
func describeUser(u *models.User) string {
	if u == nil {
		return ""
	}

	return fmt.Sprintf("%s", u.Username)
}
