package baidu

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jenken827/pfm-plugin-sdk/driver/types"
	"github.com/jenken827/pfm-plugin-sdk/plugin/sdk/logx"
	"github.com/jenken827/pfm-plugin-sdk/proto/gen/abi"
)

// fakePersister 记录配置回写（token 轮换必须持久化）
type fakePersister struct {
	mu         sync.Mutex
	calls      int
	providerID uint32
	configJSON string
}

func (p *fakePersister) UpdateConfig(providerID uint32, configJSON string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.providerID = providerID
	p.configJSON = configJSON
	return nil
}

// fakeBaidu 本地假实现：oplist renewapi + 百度官方 token + 百度网盘 xpan 接口
type fakeBaidu struct {
	mu sync.Mutex

	renewCalls   int
	tokenCalls   int
	renewQueries []map[string]string

	// validTokens 当前被 xpan 接口接受的 access_token
	validTokens map[string]bool
	// renewResp / tokenResp 返回的令牌（默认自动生成）
	renewAccess  string
	renewRefresh string
	// renewErr 非空时 renewapi 返回该错误文案（text 字段）
	renewErr string
	// renewDelay 模拟续期接口耗时（用于并发去重用例）
	renewDelay time.Duration
}

func newFakeBaidu() *fakeBaidu {
	return &fakeBaidu{validTokens: map[string]bool{}}
}

// start 启动假服务器并把包级端点指过来，返回清理函数
func (f *fakeBaidu) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.route))
	oldBase, oldToken, oldRenew := APIBaseURL, TokenURL, RenewAPIDefault
	APIBaseURL = srv.URL + "/rest/2.0"
	TokenURL = srv.URL + "/oauth/token"
	RenewAPIDefault = srv.URL + "/renewapi"
	t.Cleanup(func() {
		APIBaseURL, TokenURL, RenewAPIDefault = oldBase, oldToken, oldRenew
		srv.Close()
	})
	return srv
}

func (f *fakeBaidu) route(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/renewapi":
		f.handleRenew(w, r)
	case r.URL.Path == "/oauth/token":
		f.handleToken(w, r)
	case strings.HasPrefix(r.URL.Path, "/rest/2.0/"):
		f.handleXpan(w, r)
	default:
		http.NotFound(w, r)
	}
}

// handleRenew oplist 公共授权续期：只认 refresh_ui，不需要 client 凭据
func (f *fakeBaidu) handleRenew(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renewCalls++
	if f.renewDelay > 0 {
		time.Sleep(f.renewDelay)
	}
	q := map[string]string{
		"refresh_ui": r.URL.Query().Get("refresh_ui"),
		"server_use": r.URL.Query().Get("server_use"),
		"driver_txt": r.URL.Query().Get("driver_txt"),
	}
	f.renewQueries = append(f.renewQueries, q)

	if f.renewErr != "" {
		writeJSON(w, map[string]any{"text": f.renewErr})
		return
	}
	if f.renewAccess == "" {
		f.renewAccess = "renewed-access-" + q["refresh_ui"]
		f.renewRefresh = "renewed-refresh-" + q["refresh_ui"]
		// 只有自动生成的令牌才被视为有效；测试显式设定的令牌用来模拟「续期回来也不可用」
		f.validTokens[f.renewAccess] = true
	}
	writeJSON(w, map[string]any{
		"access_token":  f.renewAccess,
		"refresh_token": f.renewRefresh,
		"expires_in":    2592000,
	})
}

// handleToken 百度官方 OAuth2 续期：必须 client_id/client_secret + refresh_token
func (f *fakeBaidu) handleToken(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenCalls++
	q := r.URL.Query()
	if q.Get("grant_type") != "refresh_token" || q.Get("client_id") == "" ||
		q.Get("client_secret") == "" || q.Get("refresh_token") == "" {
		writeJSON(w, map[string]any{"error": "invalid_request"})
		return
	}
	access := "official-access-" + q.Get("refresh_token")
	f.validTokens[access] = true
	writeJSON(w, map[string]any{
		"access_token":  access,
		"refresh_token": "official-rotated",
	})
}

// handleXpan 百度网盘接口：token 不在白名单里就返回 errno=111
func (f *fakeBaidu) handleXpan(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	token := r.URL.Query().Get("access_token")
	valid := f.validTokens[token]
	f.mu.Unlock()

	if !valid {
		writeJSON(w, map[string]any{"errno": 111, "errmsg": "access token invalid"})
		return
	}
	writeJSON(w, map[string]any{"errno": 0, "vip_type": 2, "baidu_name": "tester"})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// newTestDriver 构造一个只依赖配置的驱动实例
func newTestDriver(t *testing.T, cfg *Config, persist *fakePersister) *BaiduNetdiskDriver {
	t.Helper()
	d := &BaiduNetdiskDriver{
		Config:     cfg,
		providerID: 7,
		client:     &http.Client{},
	}
	d.setTokens(cfg.AccessToken, cfg.RefreshToken)
	if persist != nil {
		d.SetConfigPersister(persist)
	}
	return d
}

// TestRefreshViaOpenListPublicAPI 只配 refresh_token + api_url（无 client 凭据）时
// 走 OpenList 公共授权，并按 OpenList 的参数约定请求。
func TestRefreshViaOpenListPublicAPI(t *testing.T) {
	f := newFakeBaidu()
	f.start(t)
	cfg := &Config{RefreshToken: "user-refresh-ui", APIURL: RenewAPIDefault}
	persist := &fakePersister{}
	d := newTestDriver(t, cfg, persist)

	if err := d.doRefreshToken(); err != nil {
		t.Fatalf("doRefreshToken: %v", err)
	}

	if got, want := d.token(), "renewed-access-user-refresh-ui"; got != want {
		t.Errorf("access_token = %q, want %q", got, want)
	}
	if got, want := d.refreshTokenValue(), "renewed-refresh-user-refresh-ui"; got != want {
		t.Errorf("refresh_token = %q, want %q", got, want)
	}
	if f.tokenCalls != 0 {
		t.Errorf("不应调百度官方 token 接口，实际调用 %d 次", f.tokenCalls)
	}
	if f.renewCalls != 1 {
		t.Fatalf("renewapi 调用 %d 次，want 1", f.renewCalls)
	}
	// 参数必须与 OpenList drivers/baidu_netdisk/util.go 完全一致
	q := f.renewQueries[0]
	if q["refresh_ui"] != "user-refresh-ui" || q["server_use"] != "true" || q["driver_txt"] != "baiduyun_go" {
		t.Errorf("renewapi 参数不符: %+v", q)
	}
	// 轮换后的 refresh_token 必须回写并持久化
	if persist.calls != 1 {
		t.Fatalf("持久化调用 %d 次，want 1", persist.calls)
	}
	if persist.providerID != 7 {
		t.Errorf("持久化 providerID = %d, want 7", persist.providerID)
	}
	var saved Config
	if err := json.Unmarshal([]byte(persist.configJSON), &saved); err != nil {
		t.Fatalf("回写的配置不是合法 JSON: %v", err)
	}
	if saved.RefreshToken != "renewed-refresh-user-refresh-ui" {
		t.Errorf("回写的 refresh_token = %q, want 轮换后的新值", saved.RefreshToken)
	}
	if saved.AccessToken != "renewed-access-user-refresh-ui" {
		t.Errorf("回写的 access_token = %q", saved.AccessToken)
	}
	if saved.APIURL != RenewAPIDefault {
		t.Errorf("回写不应丢 api_url: %q", saved.APIURL)
	}
}

// TestRefreshUsesRotatedToken 第二次续期必须用接口返回的新 refresh_token
func TestRefreshUsesRotatedToken(t *testing.T) {
	f := newFakeBaidu()
	f.start(t)
	cfg := &Config{RefreshToken: "first-ui", APIURL: RenewAPIDefault}
	d := newTestDriver(t, cfg, nil)

	if err := d.doRefreshToken(); err != nil {
		t.Fatalf("第一次续期: %v", err)
	}
	if err := d.doRefreshToken(); err != nil {
		t.Fatalf("第二次续期: %v", err)
	}
	if len(f.renewQueries) != 2 {
		t.Fatalf("renewapi 调用 %d 次, want 2", len(f.renewQueries))
	}
	if got := f.renewQueries[1]["refresh_ui"]; got != "renewed-refresh-first-ui" {
		t.Errorf("第二次 refresh_ui = %q, want 轮换后的新值", got)
	}
}

// TestRefreshErrorFromOpenList renewapi 返回错误文案时要透传，且不能把 token 清空
func TestRefreshErrorFromOpenList(t *testing.T) {
	f := newFakeBaidu()
	f.renewErr = "refresh token is invalid"
	f.start(t)
	cfg := &Config{RefreshToken: "bad-ui", APIURL: RenewAPIDefault}
	d := newTestDriver(t, cfg, nil)
	d.setTokens("old-access", "")

	err := d.doRefreshToken()
	if err == nil {
		t.Fatal("期望续期失败")
	}
	if !strings.Contains(err.Error(), "refresh token is invalid") {
		t.Errorf("错误信息未透传: %v", err)
	}
	if d.token() != "old-access" {
		t.Errorf("失败时不应丢弃现有 access_token，得到 %q", d.token())
	}
}

// TestRefreshViaOfficialOAuth api_url 留空时走百度官方 OAuth2（需要 client 凭据）
func TestRefreshViaOfficialOAuth(t *testing.T) {
	f := newFakeBaidu()
	f.start(t)
	cfg := &Config{
		RefreshToken: "official-refresh",
		ClientID:     "cid",
		ClientSecret: "csecret",
	}
	persist := &fakePersister{}
	d := newTestDriver(t, cfg, persist)

	if err := d.doRefreshToken(); err != nil {
		t.Fatalf("doRefreshToken: %v", err)
	}
	if f.renewCalls != 0 {
		t.Errorf("配了自有凭据且未填 api_url 时不应走 oplist，实际调用 %d 次", f.renewCalls)
	}
	if f.tokenCalls != 1 {
		t.Errorf("官方 token 接口调用 %d 次, want 1", f.tokenCalls)
	}
	if got, want := d.token(), "official-access-official-refresh"; got != want {
		t.Errorf("access_token = %q, want %q", got, want)
	}
	if got, want := d.refreshTokenValue(), "official-rotated"; got != want {
		t.Errorf("refresh_token = %q, want %q", got, want)
	}
	if persist.calls != 1 {
		t.Errorf("持久化调用 %d 次, want 1", persist.calls)
	}
}

// TestRefreshWithoutAnyCredentials 既没 api_url 也没 client 凭据时给出可操作的报错
func TestRefreshWithoutAnyCredentials(t *testing.T) {
	cfg := &Config{RefreshToken: "only-refresh"}
	d := newTestDriver(t, cfg, nil)
	err := d.doRefreshToken()
	if err == nil {
		t.Fatal("期望报错")
	}
	if !strings.Contains(err.Error(), RenewAPIDefault) {
		t.Errorf("错误信息应提示 OpenList 续期地址，得到: %v", err)
	}
}

// TestRefreshEmptyRefreshToken refresh_token 缺失时提示去哪做公共授权
func TestRefreshEmptyRefreshToken(t *testing.T) {
	d := newTestDriver(t, &Config{APIURL: RenewAPIDefault}, nil)
	err := d.doRefreshToken()
	if err == nil {
		t.Fatal("期望报错")
	}
	if !strings.Contains(err.Error(), OpenListAuthorizeURL) {
		t.Errorf("错误信息应给出 OpenList 公共授权地址，得到: %v", err)
	}
}

// TestRequestRefreshesOnExpiredToken 接口返回 errno=111 时自动续期并只重试一次
func TestRequestRefreshesOnExpiredToken(t *testing.T) {
	f := newFakeBaidu()
	f.start(t)
	cfg := &Config{RefreshToken: "ui", APIURL: RenewAPIDefault, AccessToken: "expired-access"}
	d := newTestDriver(t, cfg, nil)
	d.setTokens("expired-access", cfg.RefreshToken)

	body, err := d.get("/xpan/nas", map[string]string{"method": "uinfo"})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if !strings.Contains(string(body), `"errno":0`) {
		t.Errorf("响应不符: %s", body)
	}
	if f.renewCalls != 1 {
		t.Errorf("renewapi 调用 %d 次, want 1", f.renewCalls)
	}
}

// TestRequestDoesNotLoopWhenRefreshKeepsFailing 续期后仍 111 时不能再续期（防死循环）
func TestRequestDoesNotLoopWhenRefreshKeepsFailing(t *testing.T) {
	f := newFakeBaidu()
	f.start(t)
	// renewapi 每次都返回同一个已失效 token：xpan 永远 111
	f.renewAccess = "still-invalid"
	f.renewRefresh = "still-invalid-refresh"
	cfg := &Config{RefreshToken: "ui", APIURL: RenewAPIDefault, AccessToken: "start"}
	d := newTestDriver(t, cfg, nil)
	d.setTokens("start", cfg.RefreshToken)

	_, err := d.get("/xpan/nas", map[string]string{"method": "uinfo"})
	if err == nil {
		t.Fatal("期望报错")
	}
	if !strings.Contains(err.Error(), "still rejected after refresh") {
		t.Errorf("错误信息应说明续期后仍被拒: %v", err)
	}
	if f.renewCalls != 1 {
		t.Errorf("renewapi 调用 %d 次, want 1（不能死循环）", f.renewCalls)
	}
}

// TestRefreshIfStaleSkipsWhenAlreadyRenewed 并发续期的去重：别的协程刷新过就不再打网络
func TestRefreshIfStaleSkipsWhenAlreadyRenewed(t *testing.T) {
	f := newFakeBaidu()
	f.start(t)
	cfg := &Config{RefreshToken: "ui", APIURL: RenewAPIDefault}
	d := newTestDriver(t, cfg, nil)

	// 模拟「调用方拿着 old，但当前 token 已被其它协程换成 new」
	d.setTokens("new-access", "")
	if err := d.refreshIfStale("old-access"); err != nil {
		t.Fatalf("refreshIfStale: %v", err)
	}
	if f.renewCalls != 0 {
		t.Errorf("已经被续期过就不该再打续期接口，renewCalls=%d", f.renewCalls)
	}
	if d.token() != "new-access" {
		t.Errorf("不应改动现有 token，得到 %q", d.token())
	}
}

// TestConcurrentRefreshSingleFlight 多个协程同时因 111 续期时，只允许真正打一次续期接口
//
// 分片上传是并发的，续期会轮换 refresh_token，并发续期会让彼此的新值立刻作废。
func TestConcurrentRefreshSingleFlight(t *testing.T) {
	f := newFakeBaidu()
	f.renewDelay = 50 * time.Millisecond
	f.start(t)
	cfg := &Config{RefreshToken: "ui", APIURL: RenewAPIDefault}
	d := newTestDriver(t, cfg, nil)
	d.setTokens("expired-access", cfg.RefreshToken)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = d.refreshIfStale("expired-access")
		}()
	}
	wg.Wait()

	if f.renewCalls != 1 {
		t.Errorf("renewapi 调用 %d 次, want 1（并发续期必须去重）", f.renewCalls)
	}
	if got, want := d.token(), "renewed-access-ui"; got != want {
		t.Errorf("access_token = %q, want %q", got, want)
	}
}

// TestCheckAuthentication 认证方式预检
func TestCheckAuthentication(t *testing.T) {
	cases := []struct {
		name string
		cfg  *Config
		ok   bool
	}{
		{"access_token 兜底", &Config{AccessToken: "a"}, true},
		{"oplist 公共授权", &Config{RefreshToken: "r", APIURL: RenewAPIDefault}, true},
		{"自有应用", &Config{RefreshToken: "r", ClientID: "i", ClientSecret: "s"}, true},
		{"refresh_token 但无续期方式", &Config{RefreshToken: "r"}, false},
		{"只有 client_id", &Config{RefreshToken: "r", ClientID: "i"}, false},
		{"什么都没有", &Config{}, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkAuthentication(tc.cfg)
			if tc.ok && err != nil {
				t.Fatalf("应通过，得到 %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("应报错")
			}
		})
	}
}

// TestConfigMatchesConfigFields 元数据里的字段名必须与 Config 的 JSON 键一一对应
//
// 这是插件自有配置结构的关键不变量：前端按 ConfigField.Name 拼 JSON，驱动按
// struct tag 解析，两边对不上就是「配置存了但驱动读不到」。
func TestConfigMatchesConfigFields(t *testing.T) {
	info := metadataForTest()
	fields := map[string]bool{}
	for _, f := range info.GetConfigFields() {
		fields[f.GetName()] = true
	}

	// 用全零值序列化拿到驱动认识的全部键
	raw, err := json.Marshal(&Config{
		RefreshToken: "r", AccessToken: "a", RootPath: "/", UploadThread: "3",
		CustomUploadPartSize: "4", ClientID: "i", ClientSecret: "s", APIURL: "u",
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	var keys map[string]any
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	for k := range keys {
		if !fields[k] {
			t.Errorf("Config 的 JSON 键 %q 不在 ConfigFields 里", k)
		}
	}
	for k := range fields {
		if _, ok := keys[k]; !ok {
			t.Errorf("ConfigFields 里的 %q 没有被 Config 接收", k)
		}
	}
}

// TestConfigParsesNumericFieldBothWays NUMBER 字段的 JSON 可能是数字也可能是字符串
func TestConfigParsesNumericFieldBothWays(t *testing.T) {
	cases := []string{
		`{"upload_thread":3,"custom_upload_part_size":16}`,
		`{"upload_thread":"3","custom_upload_part_size":"16"}`,
	}
	for _, raw := range cases {
		var c Config
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			t.Fatalf("解析 %s 失败: %v", raw, err)
		}
		if c.UploadThread.Int() != 3 {
			t.Errorf("%s: upload_thread = %q", raw, c.UploadThread)
		}
		if c.CustomUploadPartSize.Int() != 16 {
			t.Errorf("%s: custom_upload_part_size = %q", raw, c.CustomUploadPartSize)
		}
	}
}

// metadataForTest 取出注册表里 baidu 驱动的元数据
func metadataForTest() *abi.DriverInfo {
	return types.GetDriverMetadata(DriverID)
}

// TestMain 收窄测试期日志：正常用例只看结果，失败时再看细节
func TestMain(m *testing.M) {
	logx.Init("baidu-test")
	logx.SetLevel("error")
	os.Exit(m.Run())
}
