package proxy

import (
	"bytes"
	"container/list"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy/basispoints"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// excelBPSReplay may be backed by the shared runtime cache; it serves only
// conversations named by a session header. excelBPSLocalReplay serves every
// other scope and never leaves this process, because content-, cache-key- and
// API-key-derived scopes can be shared by unrelated conversations.
var (
	excelBPSReplay      basispoints.ReplayCache
	excelBPSLocalReplay basispoints.ReplayCache
)

// excelBPSConversationHeaders name one conversation, unlike Idempotency-Key
// (one request) or affinity headers (a routing group).
var excelBPSConversationHeaders = []string{"Session-Id", "Session_id", "Conversation-Id", "Conversation_id", "X-Session-Id"}

// excelBPSConversationScoped reports whether the BPS replay scope identifies a
// single conversation: a session header set it and no downstream affinity
// header replaced it.
func excelBPSConversationScoped(headers http.Header, identity requestSessionIdentity) bool {
	if identity.hasDownstreamAffinity {
		return false
	}
	for _, key := range excelBPSConversationHeaders {
		if strings.TrimSpace(headers.Get(key)) != "" {
			return true
		}
	}
	return false
}

// excelBPSDo is kept as a narrow seam for focused adapter tests. Production
// requests use the account-isolated Codex transport and the account proxy.
var excelBPSDo = func(req *http.Request, account *auth.Account, proxyURL string) (*http.Response, error) {
	client := *getPooledClient(account, proxyURL)
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return client.Do(req)
}

type excelBPSHTTPError struct {
	status int
	code   string // Provider code only; never retain the upstream message or body.
	// rateLimit is the client-safe notice of a 429, carrying only the retry
	// delay; retryAfter is that delay, or 0 when the provider named none.
	rateLimit  string
	retryAfter time.Duration
	// gaveUp marks a rate limit the request already waited out in vain.
	gaveUp bool
}

func (e *excelBPSHTTPError) Error() string {
	if e == nil {
		return "Basispoints upstream request failed"
	}
	return fmt.Sprintf("Basispoints upstream returned HTTP %d", e.status)
}

type excelBPSFailure struct {
	status int
	code   string
	// detail is a local validation reason. It never carries upstream bodies or
	// credentials, so it is safe to return to the caller that sent the request.
	detail string
}

func (e *excelBPSFailure) Error() string {
	if e == nil || e.code == "" {
		return "Basispoints request failed"
	}
	return "Basispoints request failed: " + e.code
}

type excelBPSUpstream struct {
	response *http.Response
	bridge   *basispoints.Bridge
	model    string
	// encrypted scopes this conversation's rejected encrypted reasoning, and
	// digests lists the encrypted items the accepted body carried.
	encrypted encryptedScopeKey
	digests   []encryptedDigest
}

// rejectEncrypted remembers that the upstream could not read the encrypted
// reasoning this request carried, so the next attempt leaves it out. It
// reports false when there was none, since a retry would change nothing.
func (u *excelBPSUpstream) rejectEncrypted() bool {
	if u == nil || len(u.digests) == 0 {
		return false
	}
	rejectedEncryptedContent.mark(u.encrypted, u.digests)
	return true
}

// excelBPSEncryptedScope namespaces the encrypted reasoning BPS rejected by
// account credential and conversation. Reasoning another account or the
// native backend encrypted cannot be read here, and the client resends it
// with every later turn of the conversation.
func excelBPSEncryptedScope(account *auth.Account, scope string) encryptedScopeKey {
	return encryptedScopeKey{
		keyIdentity: "excel-bps",
		account:     account.ID(),
		generation:  account.GetCredentialGeneration(),
		session:     sha256.Sum256([]byte(scope)),
	}
}

// excelBPSToolImagesRefusedAt records, as Unix nanoseconds, when BPS last
// refused tool-result images sent inline and then accepted them uploaded.
// Later requests upload them from the start instead of spending a refused
// request to learn it again; after excelBPSToolImagesRefusalTTL the inline
// form is tried again in case the backend changed.
var excelBPSToolImagesRefusedAt atomic.Int64

const excelBPSToolImagesRefusalTTL = 24 * time.Hour

func excelBPSInitialImageMode() basispoints.ImageMode {
	if at := excelBPSToolImagesRefusedAt.Load(); at != 0 && time.Since(time.Unix(0, at)) < excelBPSToolImagesRefusalTTL {
		return basispoints.ImagesUploadAll
	}
	return basispoints.ImagesDefault
}

// ExcelBPSResponse is the response returned by ExecuteExcelBPSRequest. The
// caller owns Response.Body and must close it after consuming Bridge.Stream.
type ExcelBPSResponse struct {
	Response *http.Response
	Bridge   *basispoints.Bridge
	Model    string
}

// excelBPSClientHeaders identify the request as the Excel add-in on desktop
// Office, matching what the add-in sends so backend features gated on the
// client (attachments, image input) behave the same.
var excelBPSClientHeaders = [][2]string{
	{"X-Basispoints-Auth-Mode", "chatgpt"},
	{"X-Openai-Internal-Basispoints-Client-Product", "basispoints-excel-plugin"},
	{"X-Openai-Internal-Basispoints-Client-Agent-Profile", "excel"},
	{"X-Openai-Internal-Basispoints-Client-Editor", "excel"},
	{"X-Openai-Internal-Basispoints-Client-Host", "office"},
	{"X-Openai-Internal-Basispoints-Client-Platform", "excel"},
	{"X-Openai-Internal-Basispoints-Client-Platform-Class", "PC"},
	{"X-Openai-Internal-Basispoints-Client-Runtime", "desktop"},
	{"X-Openai-Internal-Basispoints-Office-Host", "Excel"},
	{"X-Openai-Internal-Basispoints-Office-Platform", "PC"},
	{"X-Stainless-Arch", "unknown"},
	{"X-Stainless-Lang", "js"},
	{"X-Stainless-Os", "Unknown"},
	{"X-Stainless-Package-Version", "6.31.0"},
	{"X-Stainless-Retry-Count", "0"},
	{"X-Stainless-Runtime", "browser:chrome"},
}

// excelBPSUserAgentEnv pins the User-Agent of every Basispoints request,
// overriding the global client identity settings.
const excelBPSUserAgentEnv = "CODEX_EXCEL_BPS_USER_AGENT"

type excelBPSUserAgentKey struct{}

// withExcelBPSUserAgent carries the User-Agent resolved for one client request
// to every BPS request made for it: Responses and attachment uploads alike.
func withExcelBPSUserAgent(ctx context.Context, userAgent string) context.Context {
	return context.WithValue(ctx, excelBPSUserAgentKey{}, userAgent)
}

func excelBPSUserAgentFrom(ctx context.Context) string {
	userAgent, _ := ctx.Value(excelBPSUserAgentKey{}).(string)
	return userAgent
}

// resolveExcelBPSUserAgent returns the User-Agent BPS requests send: the
// CODEX_EXCEL_BPS_USER_AGENT override, else the one the global client identity
// settings (codex_user_agent_config, client compatibility, device profile)
// give this account and client request, exactly as its native Codex requests
// send it, so an account presents one client on both paths.
func resolveExcelBPSUserAgent(account *auth.Account, apiKey string, deviceCfg *DeviceProfileConfig, downstream http.Header) string {
	if value := strings.TrimSpace(os.Getenv(excelBPSUserAgentEnv)); value != "" {
		return value
	}
	if downstream == nil {
		downstream = http.Header{}
	}
	userAgent, _ := ResolveCodexOutboundClientHeaders(account, apiKey, deviceCfg, downstream)
	return userAgent
}

// setExcelBPSHeaders applies the account credentials and the Excel add-in
// client identity shared by Responses and attachment requests. Callers set
// Content-Type and Accept for their own body.
func setExcelBPSHeaders(req *http.Request, token, accountID string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Chatgpt-Account-Id", accountID)
	req.Header.Set("X-Openai-Account-Id", accountID)
	// The add-in names the signed-in user of the account too; the value comes
	// from the same token, so it cannot disagree with the credentials.
	if claims := auth.ParseAccessToken(token); claims != nil && claims.ChatGPTAccountUserID != "" {
		req.Header.Set("X-Openai-Account-User-Id", claims.ChatGPTAccountUserID)
	}
	req.Header.Set("Origin", "https://bps.openai.com")
	userAgent := excelBPSUserAgentFrom(req.Context())
	if userAgent == "" {
		userAgent = resolveExcelBPSUserAgent(nil, "", nil, nil)
	}
	req.Header.Set("User-Agent", userAgent)
	for _, header := range excelBPSClientHeaders {
		req.Header.Set(header[0], header[1])
	}
}

const (
	excelBPSReplayNamespace = "excel_bps_replay"
	// Long enough for `codex resume` on a conversation from last week, short
	// enough that abandoned threads do not accumulate in Redis.
	excelBPSReplayTTL = 7 * 24 * time.Hour
	// Each lookup is bounded tightly; ReplayCache also caps the whole
	// prefetch and pauses the store after a failure.
	excelBPSReplayTimeout = 200 * time.Millisecond
)

// excelBPSReplayBacking keeps BPS native tool items in the shared runtime
// cache, so a restart or another instance replays them exactly.
type excelBPSReplayBacking struct {
	cache cache.TokenCache
}

func excelBPSReplayKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// Load reads one replay record under the hashed key. A miss returns false with
// no error; cache errors are returned so ReplayCache can pause the backing.
func (b excelBPSReplayBacking) Load(key string) ([]byte, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), excelBPSReplayTimeout)
	defer cancel()
	raw, ok, err := b.cache.GetRuntime(ctx, excelBPSReplayNamespace, excelBPSReplayKey(key))
	return raw, ok, err
}

// Store writes one replay record with excelBPSReplayTTL. It runs on
// ReplayCache's background writer, never on the request path.
func (b excelBPSReplayBacking) Store(key string, value []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), excelBPSReplayTimeout)
	defer cancel()
	return b.cache.SetRuntime(ctx, excelBPSReplayNamespace, excelBPSReplayKey(key), value, excelBPSReplayTTL)
}

// ConfigureExcelBPSReplay attaches the process-wide BPS replay cache to the
// shared runtime cache. It is called once at startup; a process-local memory
// cache adds nothing over the in-process LRU, so it leaves replay local.
func ConfigureExcelBPSReplay(tc cache.TokenCache) {
	if tc == nil || !tc.SharedAcrossInstances() {
		excelBPSReplay.SetBacking(nil)
		return
	}
	excelBPSReplay.SetBacking(excelBPSReplayBacking{cache: tc})
}

func newExcelBPSRequest(ctx context.Context, body []byte, token, accountID string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, basispoints.ResponsesURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	setExcelBPSHeaders(req, token, accountID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	return req, nil
}

// excelBPSAttachmentCache maps (account, image digest) to an uploaded file ID
// so images repeated in conversation history are uploaded once.
type excelBPSAttachmentCache struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   list.List
}

type excelBPSAttachment struct {
	key    string
	fileID string
}

const excelBPSAttachmentCacheSize = 256

func (c *excelBPSAttachmentCache) get(key string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[key]
	if entry == nil {
		return ""
	}
	c.order.MoveToBack(entry)
	return entry.Value.(excelBPSAttachment).fileID
}

func (c *excelBPSAttachmentCache) put(key, fileID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]*list.Element)
	}
	if old := c.entries[key]; old != nil {
		c.order.Remove(old)
	}
	c.entries[key] = c.order.PushBack(excelBPSAttachment{key: key, fileID: fileID})
	for len(c.entries) > excelBPSAttachmentCacheSize {
		oldest := c.order.Front()
		delete(c.entries, oldest.Value.(excelBPSAttachment).key)
		c.order.Remove(oldest)
	}
}

func (c *excelBPSAttachmentCache) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = nil
	c.order.Init()
}

// forget drops cached uploads the upstream may no longer honour.
func (c *excelBPSAttachmentCache) forget(fileIDs map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, entry := range c.entries {
		if fileIDs[entry.Value.(excelBPSAttachment).fileID] {
			delete(c.entries, key)
			c.order.Remove(entry)
		}
	}
}

var excelBPSAttachments excelBPSAttachmentCache

var excelBPSImageExtensions = map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/gif": "gif", "image/webp": "webp"}

// uploadExcelBPSImage uploads one image the way the Excel add-in's "Upload
// file" button does and returns the OpenAI file ID the Responses body names.
// mediaType has been validated as a plain image/* token by basispoints.
func uploadExcelBPSImage(ctx context.Context, account *auth.Account, proxyURL, token, accountID, mediaType string, data []byte, digest, tag string) (string, error) {
	extension := excelBPSImageExtensions[mediaType]
	if extension == "" {
		extension = "png"
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="picture-%s.%s"`, digest[:12], extension))
	header.Set("Content-Type", mediaType)
	part, err := form.CreatePart(header)
	if err == nil {
		_, err = part.Write(data)
	}
	if err == nil {
		err = form.Close()
	}
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, basispoints.AttachmentsURL, &body)
	if err != nil {
		return "", err
	}
	setExcelBPSHeaders(req, token, accountID)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	response, err := excelBPSDo(req, account, proxyURL)
	if err != nil {
		return "", fmt.Errorf("attachment upload failed: %w", err)
	}
	if response == nil {
		return "", errors.New("attachment upload returned no response")
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		basispoints.Logf("%s attachment upload HTTP %d: %s", tag, response.StatusCode, excelBPSErrorShape(payload))
		return "", fmt.Errorf("attachment upload returned HTTP %d", response.StatusCode)
	}
	fileID := strings.TrimSpace(gjson.GetBytes(payload, "openai_file_id").String())
	if fileID == "" {
		return "", errors.New("attachment upload returned no file ID")
	}
	basispoints.Logf("%s uploaded %d KB image as %s", tag, max(1, len(data)>>10), fileID)
	return fileID, nil
}

// excelBPSErrorShape summarizes a provider error body for operator logs with
// its code, type and size only; provider messages can echo request content.
func excelBPSErrorShape(body []byte) string {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return fmt.Sprintf("non-JSON body bytes=%d", len(body))
	}
	return fmt.Sprintf("%s bytes=%d", basispoints.TerminalErrorShape(payload), len(body))
}

// excelBPSImageRefusal reports whether a rejected request may have failed
// because of its images. BPS answers invalid image placement with a bare 422
// schema error; a 400 counts only when it names images or files, so unrelated
// 400s (context length, bad fields) are not retried with re-uploads.
func excelBPSImageRefusal(status int, body []byte) bool {
	if status == http.StatusUnprocessableEntity {
		return true
	}
	if status != http.StatusBadRequest {
		return false
	}
	message := strings.ToLower(gjson.GetBytes(body, "error.message").String() + " " + gjson.GetBytes(body, "detail").String())
	return strings.Contains(message, "image") || strings.Contains(message, "file") || strings.Contains(message, "attachment")
}

// nextExcelBPSImageMode picks the recovery step after BPS refuses a body with
// images: re-upload stale or inline images first, then drop them entirely.
func nextExcelBPSImageMode(report basispoints.ImageReport, uploaded map[string]bool) (basispoints.ImageMode, map[string]bool) {
	reused := make(map[string]bool)
	for _, fileID := range report.FileIDs {
		if !uploaded[fileID] {
			reused[fileID] = true
		}
	}
	// Fresh uploads always follow a re-upload, so this cannot repeat forever.
	if report.Inline || len(reused) > 0 {
		return basispoints.ImagesUploadAll, reused
	}
	return basispoints.ImagesOmit, reused
}

func excelBPSAccountID(account *auth.Account, token string) string {
	if account != nil {
		if id := strings.TrimSpace(account.EffectiveAccountID()); id != "" {
			return id
		}
	}
	if claims := auth.ParseAccessToken(token); claims != nil {
		return strings.TrimSpace(claims.ChatGPTAccountID)
	}
	return ""
}

func setExcelBPSPromptCacheKey(raw []byte, threadKey string, compact bool) ([]byte, error) {
	var source map[string]any
	if err := json.Unmarshal(raw, &source); err != nil || source == nil {
		return nil, errors.New("invalid Responses request")
	}
	if compact {
		var input []any
		switch value := source["input"].(type) {
		case []any:
			input = append(input, value...)
		case string:
			input = []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": value}}}}
		default:
			return nil, errors.New("compact request input is missing")
		}
		input = append(input, map[string]any{"type": "compaction_trigger"})
		source["input"] = input
		source["tool_choice"] = "none"
	}
	if strings.TrimSpace(threadKey) != "" {
		source["prompt_cache_key"] = threadKey
	}
	return json.Marshal(source)
}

// tag names the client request in every line logged while it is prepared.
func prepareExcelBPSUpstream(ctx context.Context, account *auth.Account, raw []byte, scope, threadKey, proxyURL string, compact, persistReplay bool, tag string) (*excelBPSUpstream, error) {
	if account == nil || !account.IsExcelBPSEnabled() {
		basispoints.Logf("%s is not enabled for Basispoints", tag)
		return nil, &excelBPSFailure{status: http.StatusBadRequest, code: "disabled"}
	}
	if excelBPSUserAgentFrom(ctx) == "" {
		// Callers without a client request (account tests) take the
		// account's identity from the global settings alone.
		ctx = withExcelBPSUserAgent(ctx, resolveExcelBPSUserAgent(account, "", nil, nil))
	}
	token := account.GetAccessToken()
	if token == "" {
		basispoints.Logf("%s has no access token", tag)
		return nil, &excelBPSFailure{status: http.StatusBadGateway, code: "auth_unavailable"}
	}
	accountID := excelBPSAccountID(account, token)
	if accountID == "" {
		basispoints.Logf("%s has no ChatGPT account ID", tag)
		return nil, &excelBPSFailure{status: http.StatusBadRequest, code: "account_identity_missing"}
	}
	preparedInput, err := setExcelBPSPromptCacheKey(raw, threadKey, compact)
	if err != nil {
		basispoints.Logf("%s request rejected before translation: %v", tag, err)
		return nil, &excelBPSFailure{status: http.StatusBadRequest, code: "request_invalid", detail: err.Error()}
	}
	replay := &excelBPSLocalReplay
	if persistReplay {
		replay = &excelBPSReplay
	}
	prepared, bridge, err := basispoints.PrepareWithTag(preparedInput, scope, replay, tag)
	if err != nil {
		basispoints.Logf("%s prepare rejected: %v", tag, err)
		return nil, &excelBPSFailure{status: http.StatusBadRequest, code: "request_unsupported", detail: err.Error()}
	}
	encrypted := excelBPSEncryptedScope(account, scope)
	if stripped := stripRememberedEncryptedContent(prepared, rejectedEncryptedContent.get(encrypted)); len(stripped) != len(prepared) {
		basispoints.Logf("%s left out encrypted reasoning the upstream rejected earlier in this conversation", tag)
		prepared = stripped
	}
	encryptedRetried := false
	uploaded := make(map[string]bool)
	upload := func(image basispoints.InlineImage) (string, error) {
		key := fmt.Sprintf("%d\x00%s", account.ID(), image.Digest)
		if fileID := excelBPSAttachments.get(key); fileID != "" {
			return fileID, nil
		}
		mediaType, data, err := image.Decode()
		if err != nil {
			return "", err
		}
		fileID, err := uploadExcelBPSImage(ctx, account, proxyURL, token, accountID, mediaType, data, image.Digest, tag)
		if err != nil {
			return "", err
		}
		excelBPSAttachments.put(key, fileID)
		uploaded[fileID] = true
		return fileID, nil
	}
	mode := excelBPSInitialImageMode()
	// refusedInline is set once BPS refused tool-result images sent inline.
	refusedInline := false
	for {
		body, images, err := basispoints.RewriteImages(prepared, mode, upload)
		if err != nil {
			basispoints.Logf("%s request body could not be built (image mode %d): %v", tag, mode, err)
			return nil, &excelBPSFailure{status: http.StatusBadGateway, code: "request_build_failed"}
		}
		if err := images.CurrentUploadErr; err != nil {
			basispoints.Logf("%s image in the latest turn was not uploaded: %v", tag, err)
			// Invalid-image messages name only the problem, so they are safe
			// and useful to return to the client.
			if errors.Is(err, basispoints.ErrInvalidImage) {
				return nil, &excelBPSFailure{status: http.StatusBadRequest, code: "request_unsupported", detail: err.Error()}
			}
			// Upload errors can name the account proxy, so the client gets a
			// generic message while the log keeps the cause.
			return nil, &excelBPSFailure{status: http.StatusBadGateway, code: "image_upload_failed"}
		}
		if images.UploadErr != nil {
			basispoints.Logf("%s history image upload failed, sending a note instead: %v", tag, images.UploadErr)
		}
		if images.InputErr != nil {
			basispoints.Logf("%s history image is invalid, sending a note instead: %v", tag, images.InputErr)
		}
		request, err := newExcelBPSRequest(ctx, body, token, accountID)
		if err != nil {
			basispoints.Logf("%s HTTP request could not be built: %v", tag, err)
			return nil, &excelBPSFailure{status: http.StatusBadGateway, code: "request_build_failed"}
		}
		response, err := excelBPSDo(request, account, proxyURL)
		if err != nil {
			// The cause stays in the log: it can name the account proxy.
			basispoints.Logf("%s upstream request failed (body_bytes=%d): %v", tag, len(body), err)
			return nil, &excelBPSFailure{status: http.StatusBadGateway, code: "transport_error"}
		}
		if response == nil {
			basispoints.Logf("%s upstream returned no response", tag)
			return nil, &excelBPSFailure{status: http.StatusBadGateway, code: "empty_response"}
		}
		if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
			if refusedInline && mode == basispoints.ImagesUploadAll {
				excelBPSToolImagesRefusedAt.Store(time.Now().UnixNano())
			}
			return &excelBPSUpstream{
				response: response, bridge: bridge, model: gjson.GetBytes(preparedInput, "model").String(),
				encrypted: encrypted, digests: encryptedPayloadDigests(body),
			}, nil
		}
		// The body stays out of the client response; operators still need the
		// provider reason to tell unsupported input from account problems.
		snippet, _ := io.ReadAll(io.LimitReader(response.Body, upstreamErrorLogBodyMaxBytes))
		_ = response.Body.Close()
		basispoints.Logf("%s upstream HTTP %d: %s", tag, response.StatusCode, excelBPSErrorShape(snippet))
		endpoint := "/v1/responses"
		if compact {
			endpoint = "/v1/responses/compact"
		}
		logExcelBPSUpstreamError(endpoint, response.StatusCode, gjson.GetBytes(preparedInput, "model").String(), account.ID(), snippet)
		if response.StatusCode == http.StatusBadRequest && !encryptedRetried && isRejectedEncryptedContentFailure(snippet) {
			// A conversation that moved between accounts, or from the native
			// backend, carries reasoning encrypted for another credential.
			// Without it the history still reads the same.
			if digests := encryptedPayloadDigests(body); len(digests) > 0 {
				rejectedEncryptedContent.mark(encrypted, digests)
				rejected := make(map[encryptedDigest]struct{}, len(digests))
				for _, digest := range digests {
					rejected[digest] = struct{}{}
				}
				prepared = stripRememberedEncryptedContent(prepared, rejected)
				encryptedRetried = true
				basispoints.Logf("%s upstream could not read %d encrypted reasoning item(s); retrying without them", tag, len(digests))
				continue
			}
		}
		if !excelBPSImageRefusal(response.StatusCode, snippet) || !images.Any() || mode == basispoints.ImagesOmit {
			failure := &excelBPSHTTPError{status: response.StatusCode, code: gjson.GetBytes(snippet, "error.code").String()}
			failure.rateLimit, failure.retryAfter = basispoints.RateLimitNotice(snippet, response.Header, response.StatusCode)
			return nil, failure
		}
		refusedInline = refusedInline || images.Inline
		next, stale := nextExcelBPSImageMode(images, uploaded)
		excelBPSAttachments.forget(stale)
		basispoints.Logf("%s upstream refused images (inline=%t file_ids=%d stale=%d); retrying with image mode %d", tag, images.Inline, len(images.FileIDs), len(stale), next)
		mode = next
	}
}

// ExecuteExcelBPSRequest sends one prepared BPS request for account-test code
// and other non-handler callers. It intentionally exposes only the response
// stream and bridge; credentials and wire construction remain private.
func ExecuteExcelBPSRequest(ctx context.Context, account *auth.Account, raw []byte, scope, threadKey, proxyURL string, compact bool) (*ExcelBPSResponse, error) {
	// Account tests use synthetic threads; their replay stays in memory.
	upstream, err := prepareExcelBPSUpstream(ctx, account, raw, scope, threadKey, proxyURL, compact, false, newExcelBPSLogTag(account))
	if err != nil {
		return nil, err
	}
	return &ExcelBPSResponse{Response: upstream.response, Bridge: upstream.bridge, Model: upstream.model}, nil
}

type excelBPSResult struct {
	StatusCode       int
	Terminal         string
	ResponseID       string
	Model            string
	UpstreamModel    string
	RequestID        string
	DurationMs       int
	FirstTokenMs     int
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	ReasoningTokens  int
	CachedTokens     int
	ClientDisconnect bool
	// Synthesized marks a completion rebuilt after the upstream closed early;
	// it has no usage, so its token counts are unknown rather than zero.
	Synthesized bool
	// Failure is the provider's unsuccessful terminal event, if any.
	Failure basispoints.TerminalFailure
	// TranslationFailure is why the bridge failed a response it could not
	// translate, or "" when the terminal event came from the provider.
	TranslationFailure string
	// LogTag names the account and request in operator logs.
	LogTag string
	// RateLimitWaited is how long the request waited out rate limits.
	RateLimitWaited time.Duration
}

func (r *excelBPSResult) usageFrom(payload []byte) {
	response := gjson.GetBytes(payload, "response")
	if !response.Exists() {
		return
	}
	r.ResponseID = response.Get("id").String()
	r.UpstreamModel = response.Get("model").String()
	usage := response.Get("usage")
	r.PromptTokens = int(usage.Get("input_tokens").Int())
	r.CompletionTokens = int(usage.Get("output_tokens").Int())
	r.TotalTokens = int(usage.Get("total_tokens").Int())
	r.ReasoningTokens = int(usage.Get("output_tokens_details.reasoning_tokens").Int())
	r.CachedTokens = int(usage.Get("input_tokens_details.cached_tokens").Int())
}

func excelBPSTerminal(kind string) bool {
	switch kind {
	case "response.completed", "response.incomplete", "response.failed", "error":
		return true
	default:
		return false
	}
}

func writeExcelBPSFrame(w io.Writer, event string, data []byte) error {
	if event == "" {
		event = gjson.GetBytes(data, "type").String()
	}
	if event != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}

// forwardExcelBPS writes exactly one terminal outcome. BPS is always requested
// upstream as SSE, while non-stream Responses callers receive the completed
// response object after the terminal event is validated.
//
// Rate limits that arrive before any output are waited out and retried on the
// same account. Basispoints shares one tokens-per-minute budget per model among
// everyone using the Excel add-in, so rotating or freezing the account would
// not help, and the millisecond waits its hints ask for are gone in under a
// second of client retries while the budget stays spent for the minute.
const (
	// excelBPSRateLimitWaitEnv sets, in seconds, how long one request may wait
	// out rate limits before its client hears of them; 0 relays them at once.
	excelBPSRateLimitWaitEnv     = "CODEX_EXCEL_BPS_RATE_LIMIT_WAIT"
	excelBPSDefaultRateLimitWait = 5 * time.Minute
	excelBPSMaxRateLimitWait     = 30 * time.Minute
	// excelBPSRateLimitKeepalive is the longest a waiting stream stays silent.
	// Codex drops a stream that sends nothing for five minutes, and proxies in
	// front of the gateway often give up much sooner.
	excelBPSRateLimitKeepalive = 10 * time.Second
	// excelBPSHoldWindow bounds how long the opening response.created and
	// response.in_progress frames are held back while no retry has needed
	// them: a retry within it stays invisible to the client.
	excelBPSHoldWindow = 10 * time.Second
	// excelBPSRateLimitGaveUpCode reports a rate limit this request already
	// waited out in vain. Codex retries rate_limit_exceeded on its own, and
	// every retry would wait the whole budget again; invalid_prompt is one it
	// shows to the user as is instead.
	excelBPSRateLimitGaveUpCode = "invalid_prompt"
)

// excelBPSRateLimitBackoff is the least wait before each retry, raised to the
// provider's hint when that is longer; the last step repeats.
var excelBPSRateLimitBackoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second}

// excelBPSSleep waits between rate-limit retries; tests replace it.
var excelBPSSleep = func(ctx context.Context, wait time.Duration) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// excelBPSNow is the clock for hold and keepalive timing; tests replace it.
var excelBPSNow = time.Now

// excelBPSRateLimitBudget returns how long one request may wait out rate
// limits: CODEX_EXCEL_BPS_RATE_LIMIT_WAIT seconds, capped at 30 minutes, or 5
// minutes when it is unset or invalid.
func excelBPSRateLimitBudget() time.Duration {
	raw := strings.TrimSpace(os.Getenv(excelBPSRateLimitWaitEnv))
	if raw == "" {
		return excelBPSDefaultRateLimitWait
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(seconds) || seconds < 0 {
		return excelBPSDefaultRateLimitWait
	}
	if seconds >= excelBPSMaxRateLimitWait.Seconds() {
		return excelBPSMaxRateLimitWait
	}
	return time.Duration(seconds * float64(time.Second))
}

// excelBPSRateLimitWait returns the wait before rate-limit retry attempt
// (0-based) after a limit whose provider hint was hint, or false when the
// budget left cannot cover the hint.
func excelBPSRateLimitWait(hint time.Duration, attempt int, left time.Duration) (time.Duration, bool) {
	if left <= 0 || hint > left {
		return 0, false
	}
	step := excelBPSRateLimitBackoff[min(attempt, len(excelBPSRateLimitBackoff)-1)]
	return min(max(hint, step), left), true
}

// excelBPSGaveUpMessage is the client notice of a rate limit that outlasted
// the wait. It names no provider detail.
func excelBPSGaveUpMessage(waited time.Duration) string {
	return fmt.Sprintf("%s This request waited %s for the shared Basispoints rate limit to clear and was still limited; send it again later.",
		basispoints.RateLimitMessage, waited.Round(time.Second))
}

// excelBPSGaveUpFrame rewrites a sanitized rate-limit terminal frame so the
// client shows it rather than retrying: the request already waited.
func excelBPSGaveUpFrame(data []byte, waited time.Duration) []byte {
	path := "error"
	if gjson.GetBytes(data, "response.error").Exists() {
		path = "response.error"
	}
	out, err := sjson.SetBytes(data, path+".code", excelBPSRateLimitGaveUpCode)
	if err == nil {
		out, err = sjson.SetBytes(out, path+".message", excelBPSGaveUpMessage(waited))
	}
	if err != nil {
		return data
	}
	return out
}

// excelBPSRetry is what forwardExcelBPS does after one attempt.
type excelBPSRetry int

const (
	excelBPSDone excelBPSRetry = iota
	// excelBPSRetryRateLimit waits out a rate limit, then sends the request again.
	excelBPSRetryRateLimit
	// excelBPSRetryEncrypted sends the request again at once, without the
	// encrypted reasoning the upstream could not read.
	excelBPSRetryEncrypted
)

// excelBPSAttemptPlan is what one attempt may still retry.
type excelBPSAttemptPlan struct {
	// attempt counts the rate-limit retries so far.
	attempt int
	// waited is the time spent waiting out rate limits so far, and left what
	// remains of the budget.
	waited, left time.Duration
	// encryptedRetry is set while no attempt has left out rejected reasoning.
	encryptedRetry bool
}

// rateLimitRetry returns the wait before retrying a rate limit that arrived
// before any output, or false when it goes to the client, logging why.
func (p excelBPSAttemptPlan) rateLimitRetry(tag string, hint time.Duration) (time.Duration, bool) {
	wait, ok := excelBPSRateLimitWait(hint, p.attempt, p.left)
	switch {
	case ok:
	case p.left <= 0 && p.waited > 0:
		basispoints.Logf("%s still rate limited after waiting %s; returning it to the client", tag, p.waited)
	case p.left <= 0:
		basispoints.Logf("%s rate limited; waiting is disabled, returning it to the client", tag)
	default:
		basispoints.Logf("%s rate limited; provider wait %s exceeds the %s left to wait, returning it to the client", tag, hint, p.left)
	}
	return wait, ok
}

// gaveUp reports whether a rate limit reaching the client was already waited on.
func (p excelBPSAttemptPlan) gaveUp() bool {
	return p.waited > 0
}

func forwardExcelBPS(ctx context.Context, c *gin.Context, account *auth.Account, raw []byte, scope, threadKey, proxyURL string, compact, stream, persistReplay bool) (excelBPSResult, error) {
	start := time.Now()
	tag := newExcelBPSLogTag(account)
	client := newExcelBPSClient(c, stream, gjson.GetBytes(raw, "model").String())
	budget := excelBPSRateLimitBudget()
	plan := excelBPSAttemptPlan{left: budget, encryptedRetry: true}
	for {
		result, wait, next, err := forwardExcelBPSAttempt(ctx, c, client, account, raw, scope, threadKey, proxyURL, compact, stream, persistReplay, start, plan, tag)
		result.LogTag = tag
		result.RateLimitWaited = plan.waited
		switch next {
		case excelBPSRetryEncrypted:
			basispoints.Logf("%s upstream could not read the conversation's encrypted reasoning; retrying without it", tag)
			plan.encryptedRetry = false
			continue
		case excelBPSRetryRateLimit:
		default:
			return result, err
		}
		basispoints.Logf("%s rate limited before any output; retrying in %s (retry %d, waited %s of %s)", tag, wait, plan.attempt+1, plan.waited, budget)
		if err := client.wait(ctx, wait); err != nil {
			basispoints.Logf("%s client went away during the rate-limit wait: %v", tag, err)
			result.ClientDisconnect = true
			return result, err
		}
		plan.attempt++
		plan.waited += wait
		plan.left -= wait
	}
}

// newExcelBPSLogTag names one client request in operator logs. The random
// part ties the bridge's detail lines to the request summary across retries.
func newExcelBPSLogTag(account *auth.Account) string {
	var id [4]byte
	_, _ = rand.Read(id[:])
	return fmt.Sprintf("account=%d req=%s", excelBPSLogAccountID(account), hex.EncodeToString(id[:]))
}

func excelBPSLogAccountID(account *auth.Account) int64 {
	if account == nil {
		return 0
	}
	return account.ID()
}

// excelBPSFrame is one SSE frame held for the client.
type excelBPSFrame struct {
	event string
	data  []byte
}

// excelBPSClient writes one client response across the attempts of a request.
// Until the client has its response.created, opening frames are held so that a
// retried attempt can replace them unseen. Once it has one, a later attempt's
// response.created is dropped, so the client sees a single response, and its
// sequence numbers continue from where the client is.
type excelBPSClient struct {
	c       *gin.Context
	flusher http.Flusher
	stream  bool
	model   string
	start   time.Time
	// lastWrite is when the client last got a frame, zero before the first.
	lastWrite time.Time
	opened    bool
	held      []excelBPSFrame
	// response is the latest response object seen in an opening frame; the
	// keepalive frames repeat it.
	response json.RawMessage
	// sequence is the next sequence_number the client has not seen.
	sequence int64
	err      error
}

func newExcelBPSClient(c *gin.Context, stream bool, model string) *excelBPSClient {
	flusher, _ := c.Writer.(http.Flusher)
	return &excelBPSClient{c: c, flusher: flusher, stream: stream, model: model, start: excelBPSNow()}
}

func (w *excelBPSClient) idleSince() time.Time {
	if w.lastWrite.IsZero() {
		return w.start
	}
	return w.lastWrite
}

// beginAttempt starts a new upstream attempt. Opening frames of an earlier
// attempt the client never saw are dropped; the new attempt sends its own.
func (w *excelBPSClient) beginAttempt(bridge *basispoints.Bridge) {
	if !w.opened {
		w.held = nil
	}
	bridge.StartSequenceAt(int(w.sequence))
}

// opening handles a response.created or response.in_progress frame that comes
// before any output. It returns false once the client is gone.
func (w *excelBPSClient) opening(event string, data []byte) bool {
	if response := gjson.GetBytes(data, "response"); response.IsObject() {
		w.response = json.RawMessage(response.Raw)
	}
	if w.opened {
		if gjson.GetBytes(data, "type").String() == "response.created" {
			return true
		}
		return w.send(event, data)
	}
	w.held = append(w.held, excelBPSFrame{event: event, data: append([]byte(nil), data...)})
	if excelBPSNow().Sub(w.idleSince()) >= excelBPSHoldWindow {
		return w.open()
	}
	return true
}

// write sends one frame after any held opening frames. It returns false once
// the client is gone.
func (w *excelBPSClient) write(event string, data []byte) bool {
	return w.open() && w.send(event, data)
}

// open gives the client its response.created: the held opening frames, or a
// frame made here when no upstream attempt has opened a response yet.
func (w *excelBPSClient) open() bool {
	if w.opened {
		return w.err == nil
	}
	w.opened = true
	held := w.held
	w.held = nil
	if len(held) == 0 {
		held = []excelBPSFrame{{event: "response.created", data: w.frame("response.created")}}
	}
	for _, frame := range held {
		if !w.send(frame.event, frame.data) {
			return false
		}
	}
	return true
}

// keepalive tells a waiting client the response is still in progress.
func (w *excelBPSClient) keepalive() bool {
	if !w.opened {
		return w.open()
	}
	return w.send("response.in_progress", w.frame("response.in_progress"))
}

// frame builds an opening or keepalive frame around the latest response
// object, or around a new one when the upstream has not sent any.
func (w *excelBPSClient) frame(kind string) []byte {
	if w.response == nil {
		var id [12]byte
		_, _ = rand.Read(id[:])
		w.response, _ = json.Marshal(map[string]any{
			"id": "resp_" + hex.EncodeToString(id[:]), "object": "response", "created_at": time.Now().Unix(),
			"status": "in_progress", "model": w.model, "output": []any{},
		})
	}
	data, _ := json.Marshal(map[string]any{"type": kind, "sequence_number": w.sequence, "response": w.response})
	return data
}

func (w *excelBPSClient) send(event string, data []byte) bool {
	if w.err != nil {
		return false
	}
	// SSE headers wait for the first frame: while frames are held, a failure
	// is still answered as JSON, which must not carry an SSE content type.
	if !w.c.Writer.Written() {
		w.c.Header("Content-Type", "text/event-stream")
		w.c.Header("Cache-Control", "no-cache")
		w.c.Header("X-Accel-Buffering", "no")
	}
	if w.err = writeExcelBPSFrame(w.c.Writer, event, data); w.err != nil {
		return false
	}
	if w.flusher != nil {
		w.flusher.Flush()
	}
	w.lastWrite = excelBPSNow()
	if sequence := gjson.GetBytes(data, "sequence_number"); sequence.Exists() && sequence.Int() >= w.sequence {
		w.sequence = sequence.Int() + 1
	}
	return true
}

// wait sleeps for d. A streaming client gets a keepalive whenever it would
// otherwise stay silent for excelBPSRateLimitKeepalive, opening its response
// first if needed, so neither it nor a proxy in front times out.
func (w *excelBPSClient) wait(ctx context.Context, d time.Duration) error {
	for d > 0 {
		step := d
		if w.stream {
			due := excelBPSRateLimitKeepalive - excelBPSNow().Sub(w.idleSince())
			if due <= 0 {
				if !w.keepalive() {
					return w.err
				}
				due = excelBPSRateLimitKeepalive
			}
			step = min(step, due)
		}
		if err := excelBPSSleep(ctx, step); err != nil {
			return err
		}
		d -= step
	}
	return nil
}

// forwardExcelBPSAttempt sends the request once. It returns a retry instead of
// relaying a rate limit that the plan can still wait out, or an upstream
// refusal of the conversation's encrypted reasoning, when either arrives
// before any output.
func forwardExcelBPSAttempt(ctx context.Context, c *gin.Context, client *excelBPSClient, account *auth.Account, raw []byte, scope, threadKey, proxyURL string, compact, stream, persistReplay bool, start time.Time, plan excelBPSAttemptPlan, tag string) (excelBPSResult, time.Duration, excelBPSRetry, error) {
	result := excelBPSResult{}
	upstream, err := prepareExcelBPSUpstream(ctx, account, raw, scope, threadKey, proxyURL, compact, persistReplay, tag)
	if err != nil {
		var limited *excelBPSHTTPError
		if errors.As(err, &limited) && limited.rateLimit != "" {
			if wait, ok := plan.rateLimitRetry(tag, limited.retryAfter); ok {
				return result, wait, excelBPSRetryRateLimit, err
			}
			if plan.gaveUp() {
				limited.rateLimit, limited.gaveUp = excelBPSGaveUpMessage(plan.waited), true
			}
		}
		return result, 0, excelBPSDone, err
	}
	client.beginAttempt(upstream.bridge)
	defer upstream.response.Body.Close()
	result.StatusCode = upstream.response.StatusCode
	result.Model = upstream.model
	result.RequestID = upstream.response.Header.Get("x-request-id")
	converted := upstream.bridge.Stream(upstream.response.Body)
	defer converted.Close()
	var completed []byte
	outputStarted := false
	gaveUp := false
	next, retryWait := excelBPSDone, time.Duration(0)
	terminalSeen := false
	readErr := ReadSSEStreamWithEvent(converted, func(event string, data []byte) bool {
		if terminalSeen {
			return false
		}
		kind := gjson.GetBytes(data, "type").String()
		if !outputStarted && (kind == "response.created" || kind == "response.in_progress") {
			if stream && !client.opening(event, data) {
				result.ClientDisconnect = true
				return false
			}
			return true
		}
		if !outputStarted && excelBPSTerminal(kind) {
			failure := upstream.bridge.UpstreamFailure()
			if failure.RateLimit != "" {
				if wait, ok := plan.rateLimitRetry(tag, failure.RetryAfter); ok {
					next, retryWait = excelBPSRetryRateLimit, wait
					return false
				}
				if plan.gaveUp() {
					data, gaveUp = excelBPSGaveUpFrame(data, plan.waited), true
				}
			} else if plan.encryptedRetry && len(failure.Raw) > 0 && isRejectedEncryptedContentFailure(responseFailedErrorBody(failure.Raw)) && upstream.rejectEncrypted() {
				next = excelBPSRetryEncrypted
				return false
			}
		}
		outputStarted = true
		if result.FirstTokenMs == 0 && (kind == "response.output_text.delta" || kind == "response.output_item.added") {
			result.FirstTokenMs = int(time.Since(start).Milliseconds())
		}
		if excelBPSTerminal(kind) {
			terminalSeen = true
			result.Terminal = kind
			if kind == "response.completed" {
				status := strings.ToLower(strings.TrimSpace(gjson.GetBytes(data, "response.status").String()))
				if status == "failed" || status == "incomplete" {
					result.Terminal = "response." + status
				}
			}
			result.usageFrom(data)
			if !stream {
				completed = append(completed[:0], data...)
				return false
			}
		}
		if stream && !client.write(event, data) {
			result.ClientDisconnect = true
			return false
		}
		return !terminalSeen
	})
	if next != excelBPSDone {
		return result, retryWait, next, nil
	}
	result.DurationMs = int(time.Since(start).Milliseconds())
	result.Synthesized = upstream.bridge.SynthesizedCompletion()
	result.Failure = upstream.bridge.UpstreamFailure()
	result.TranslationFailure = upstream.bridge.TranslationFailure()
	if client.err != nil {
		return result, 0, excelBPSDone, client.err
	}
	if ctx.Err() != nil {
		result.ClientDisconnect = true
		return result, 0, excelBPSDone, ctx.Err()
	}
	if readErr != nil {
		return result, 0, excelBPSDone, readErr
	}
	if !terminalSeen {
		return result, 0, excelBPSDone, errors.New("Basispoints stream ended before a terminal event")
	}
	if !stream {
		if result.Terminal != "response.completed" {
			if notice := result.Failure.RateLimit; notice != "" {
				if gaveUp {
					notice = excelBPSGaveUpMessage(plan.waited)
				}
				return result, 0, excelBPSDone, &excelBPSFailure{status: http.StatusTooManyRequests, code: "rate_limited", detail: notice}
			}
			if code := result.Failure.ClientCode; code != "" {
				return result, 0, excelBPSDone, &excelBPSHTTPError{status: http.StatusBadRequest, code: code}
			}
			return result, 0, excelBPSDone, errors.New("Basispoints response did not complete")
		}
		response := gjson.GetBytes(completed, "response")
		if !response.Exists() {
			return result, 0, excelBPSDone, errors.New("Basispoints response did not contain a completed response")
		}
		c.Data(http.StatusOK, "application/json", []byte(response.Raw))
	}
	return result, 0, excelBPSDone, nil
}

// logExcelBPSUpstreamError writes a Basispoints upstream failure to the error
// log files. Unlike logUpstreamError it also keeps 401/403/429: the client
// only gets a sanitized message, so the file is where operators read the cause.
func logExcelBPSUpstreamError(endpoint string, status int, model string, accountID int64, body []byte) {
	if status >= http.StatusInternalServerError {
		serverErrorLogger.writeEntry(endpoint, status, model, accountID, body)
		return
	}
	badRequestLogger.writeEntry(endpoint, status, model, accountID, body)
}

func excelBPSFailureInfo(err error) (int, string, string) {
	var upstream *excelBPSHTTPError
	if errors.As(err, &upstream) {
		status := upstream.status
		if upstream.rateLimit != "" {
			return status, "rate_limit_exceeded", upstream.rateLimit
		}
		if status >= http.StatusInternalServerError {
			status = http.StatusBadGateway
		}
		if message, ok := basispoints.ClientError(upstream.code); ok && status < http.StatusInternalServerError {
			// Codex acts on these codes (it compacts a full context window), so
			// the code goes through; the provider message never does.
			return status, upstream.code, message
		}
		return status, "basispoints_upstream_error", "Basispoints upstream rejected the request"
	}
	var failure *excelBPSFailure
	if errors.As(err, &failure) && failure != nil {
		status := failure.status
		if status == 0 {
			status = http.StatusBadGateway
		}
		switch failure.code {
		case "request_invalid", "request_unsupported":
			message := "Basispoints does not support this request"
			if failure.detail != "" {
				message += ": " + failure.detail
			}
			return status, "basispoints_request_invalid", message
		case "rate_limited":
			return status, "rate_limit_exceeded", failure.detail
		case "image_upload_failed":
			return status, "basispoints_image_upload_failed", "Basispoints could not upload an image from the latest turn; retry the request"
		case "account_identity_missing":
			return status, "basispoints_account_id_missing", "The selected account has no Basispoints identity"
		case "auth_unavailable":
			return status, "basispoints_auth_unavailable", "The selected account OAuth credential is unavailable"
		default:
			return status, "basispoints_transport_error", "Basispoints connection failed"
		}
	}
	return http.StatusBadGateway, "basispoints_transport_error", "Basispoints connection failed"
}

func writeExcelBPSFailure(c *gin.Context, stream bool, status int, code, message string) {
	if c == nil {
		return
	}
	if !stream || !c.Writer.Written() {
		c.JSON(status, gin.H{"error": gin.H{"type": "upstream_error", "code": code, "message": message}})
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"type": "response.failed",
		"response": map[string]any{
			"status": "failed", "output": []any{},
			"error": map[string]string{"code": code, "message": message},
		},
	})
	_ = writeExcelBPSFrame(c.Writer, "response.failed", payload)
	if flusher, ok := c.Writer.(http.Flusher); ok {
		flusher.Flush()
	}
}

const excelBPSNativeFallbackKey = "codex2api.excel_bps_native_fallback"

// excelBPSNativeRequestReason identifies opaque client context which the Excel
// schema cannot represent. Plaintext agent tasks are normalized by the bridge;
// genuinely opaque context is preserved and routed to native Codex.
func excelBPSNativeRequestReason(raw []byte) string {
	if gjson.GetBytes(raw, "previous_response_id").String() != "" {
		return "stored_response"
	}
	for _, item := range gjson.GetBytes(raw, "input").Array() {
		isAgent := item.Get("type").String() == "agent_message"
		for _, field := range []string{"content", "output"} {
			for _, part := range item.Get(field).Array() {
				if part.Get("type").String() == "encrypted_content" {
					if isAgent && field == "content" && !part.Get("text").Exists() && basispoints.IsPlaintextAgentContent(part.Get("encrypted_content").String()) {
						continue
					}
					if isAgent {
						return "agent_context"
					}
					return "opaque_context"
				}
			}
		}
	}
	return ""
}

// Only retry before anything has been sent to the client. Authentication errors,
// generic forbidden responses and failures after partial output stay visible.
func excelBPSNativeFailureReason(err error) string {
	var upstream *excelBPSHTTPError
	if errors.As(err, &upstream) {
		if upstream.status >= 500 && upstream.status <= 599 {
			return "upstream_5xx"
		}
		if upstream.status == http.StatusForbidden && upstream.code == "basispoints_model_access_changed" {
			return "model_access"
		}
	}
	var failure *excelBPSFailure
	if errors.As(err, &failure) {
		switch failure.code {
		case "request_unsupported":
			return "unsupported_request"
		case "transport_error", "empty_response":
			return "transport_error"
		}
	}
	return ""
}

// tag is the request's log tag, or "" when the request never reached BPS.
func markExcelBPSNativeFallback(c *gin.Context, account *auth.Account, reason, tag string) {
	c.Set(excelBPSNativeFallbackKey, reason)
	c.Header("X-Codex2api-Upstream-Fallback", "basispoints-to-codex")
	if tag == "" {
		tag = fmt.Sprintf("account=%d", account.ID())
	}
	basispoints.Logf("%s native fallback reason=%s before_output=true", tag, reason)
}

// handleExcelBPS returns false when the normal Codex handler must continue with
// the same acquired account and original body. It must not release that account
// or write a response on fallback. The context marker prevents BPS retry loops.

// It deliberately does not report provider failures to the account scheduler:
// BPS is an opt-in alternate provider surface, not a Codex health probe.
func (h *Handler) handleExcelBPS(c *gin.Context, account *auth.Account, raw []byte, scope, threadKey, proxyURL string, compact, stream, persistReplay bool, endpoint, logModel, effectiveModel, reasoningEffort string, affinityKey string, affinityGuard auth.SessionAffinityGuard, start time.Time) bool {
	if c.GetString(excelBPSNativeFallbackKey) != "" {
		return false
	}
	if c.Request.Context().Err() == nil && !c.Writer.Written() {
		if reason := excelBPSNativeRequestReason(raw); reason != "" {
			markExcelBPSNativeFallback(c, account, reason, "")
			return false
		}
	}
	// The BPS requests present the client native Codex requests would.
	apiKey := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
	var deviceCfg *DeviceProfileConfig
	if h != nil {
		deviceCfg = h.deviceCfg
	}
	ctx := withExcelBPSUserAgent(c.Request.Context(), resolveExcelBPSUserAgent(account, apiKey, deviceCfg, c.Request.Header.Clone()))
	result, err := forwardExcelBPS(ctx, c, account, raw, scope, threadKey, proxyURL, compact, stream, persistReplay)
	if result.DurationMs == 0 && !start.IsZero() {
		result.DurationMs = int(max(int64(0), time.Since(start).Milliseconds()))
	}
	statusCode := result.StatusCode
	if statusCode == 0 {
		statusCode = http.StatusOK
	}
	logInput := &database.UsageLogInput{
		AccountID: account.ID(), Model: logModel, EffectiveModel: effectiveModel,
		Endpoint: endpoint, InboundEndpoint: endpoint, UpstreamEndpoint: basispoints.ResponsesURL,
		StatusCode: statusCode, DurationMs: result.DurationMs, FirstTokenMs: result.FirstTokenMs,
		PromptTokens: result.PromptTokens, CompletionTokens: result.CompletionTokens,
		TotalTokens: result.TotalTokens, InputTokens: result.PromptTokens, OutputTokens: result.CompletionTokens,
		ReasoningTokens: result.ReasoningTokens, CachedTokens: result.CachedTokens,
		ReasoningEffort: reasoningEffort, Stream: stream, Compact: compact,
		RequestID: result.RequestID, UpstreamResponseModel: result.UpstreamModel,
	}
	if err != nil {
		status, code, message := excelBPSFailureInfo(err)
		logInput.StatusCode = status
		logInput.UpstreamErrorKind = code
		logInput.ErrorMessage = message
		if reason := excelBPSNativeFailureReason(err); reason != "" && !result.ClientDisconnect && c.Request.Context().Err() == nil && !c.Writer.Written() {
			markExcelBPSNativeFallback(c, account, reason, result.LogTag)
			logInput.IsRetryAttempt = true
			if h != nil {
				h.logUsageForRequest(c, logInput)
			}
			return false
		}
		if !result.ClientDisconnect {
			clientCode := code
			var limited *excelBPSHTTPError
			if stream && c.Writer.Written() && errors.As(err, &limited) && limited.gaveUp {
				// The stream is already open, so the client reads the code.
				clientCode = excelBPSRateLimitGaveUpCode
			}
			writeExcelBPSFailure(c, stream, status, clientCode, message)
		}
	}
	if result.Synthesized && logInput.UpstreamErrorKind == "" {
		// Status stays 200; the kind makes the missing usage visible in logs.
		logInput.UpstreamErrorKind = "basispoints_cutoff_completed"
	}
	if reason := result.TranslationFailure; reason != "" {
		// The bridge failed the response itself; the provider's own terminal
		// event, if any, was never relayed, so name the translation cause.
		logInput.UpstreamErrorKind = "basispoints_protocol_error"
		logInput.ErrorMessage = "Basispoints response could not be translated: " + reason
	}
	if result.Terminal != "response.completed" && result.Terminal != "" && logInput.ErrorMessage == "" {
		logInput.UpstreamErrorKind = "basispoints_terminal_" + strings.TrimPrefix(result.Terminal, "response.")
		logInput.ErrorMessage = "Basispoints returned a non-completed terminal event"
	}
	if failure := result.Failure; len(failure.Raw) > 0 {
		if logInput.ErrorMessage != "" {
			logInput.ErrorMessage += " (upstream " + failure.Shape + ")"
		}
		outcome := classifyResponseFailedOutcome(failure.Raw)
		logExcelBPSUpstreamError(endpoint, outcome.logStatusCode, logModel, account.ID(), responseFailedErrorBody(failure.Raw))
	}
	if err != nil || result.Terminal != "response.completed" || result.Synthesized {
		tag := result.LogTag
		if tag == "" {
			tag = fmt.Sprintf("account=%d", account.ID())
		}
		basispoints.Logf("%s %s ended unsuccessfully: model=%s stream=%t compact=%t terminal=%q kind=%q message=%q status=%d upstream_status=%d request_id=%q client_disconnect=%t rate_limit_waited=%s duration=%dms err=%v",
			tag, endpoint, logModel, stream, compact, result.Terminal, logInput.UpstreamErrorKind, logInput.ErrorMessage,
			logInput.StatusCode, result.StatusCode, result.RequestID, result.ClientDisconnect, result.RateLimitWaited, result.DurationMs, err)
	}
	if h != nil {
		h.logUsageForRequest(c, logInput)
	}
	if err == nil && result.Terminal == "response.completed" {
		if h != nil && h.store != nil {
			h.store.ReleaseForSessionWithGuard(account, affinityKey, affinityGuard)
		}
		return true
	}
	if h != nil && h.store != nil {
		h.store.UnbindSessionAffinity(affinityKey, account.ID())
		h.store.Release(account)
	}
	return true
}
