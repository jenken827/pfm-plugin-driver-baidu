package baidu

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jenken827/pfm-plugin-sdk/plugin/sdk/logx"
)

// token 读取当前 access_token（并发安全）
func (d *BaiduNetdiskDriver) token() string {
	d.tokenMu.RLock()
	defer d.tokenMu.RUnlock()
	return d.accessToken
}

// refreshTokenValue 读取当前 refresh_token（并发安全）
func (d *BaiduNetdiskDriver) refreshTokenValue() string {
	d.tokenMu.RLock()
	defer d.tokenMu.RUnlock()
	return d.refreshToken
}

// setTokens 写入令牌；refresh 为空表示保持原值
func (d *BaiduNetdiskDriver) setTokens(access, refresh string) {
	d.tokenMu.Lock()
	defer d.tokenMu.Unlock()
	if access != "" || d.accessToken == "" {
		d.accessToken = access
	}
	if refresh != "" {
		d.refreshToken = refresh
	}
}

// refreshIfStale 续期入口：只有「调用方拿到的 token 仍是当前 token」时才真正续期
//
// 分片上传是并发的，多个 goroutine 可能同时收到 errno=111，各自调用本函数。
// 两条续期路径都会轮换 refresh_token（旧的立即失效），并发续期会让彼此刚拿到
// 的新值立刻作废，所以这里用 refreshMu 串行化，并用 stale 快照判断是否已被别的
// 协程续期过（是则直接返回，调用方用新 token 重试即可）。
func (d *BaiduNetdiskDriver) refreshIfStale(stale string) error {
	d.refreshMu.Lock()
	defer d.refreshMu.Unlock()
	if cur := d.token(); cur != "" && cur != stale {
		return nil
	}
	return d.doRefreshToken()
}

// doRefreshToken 刷新 Token（调用方须持有 refreshMu，或确保无并发）
//
// 两条路径（与 OpenList drivers/baidu_netdisk/util.go 一致）：
//  1. 配了续期服务地址（api_url，默认 OpenList 公共授权）：GET renewapi，
//     只需 refresh_token 作为 refresh_ui，**不需要自有 client_id/client_secret**；
//     返回体带轮换后的 refresh_token，必须持久化，否则下次续期用旧值会失败；
//  2. api_url 为空：调百度官方 OAuth2，必须 client_id + client_secret。
func (d *BaiduNetdiskDriver) doRefreshToken() error {
	refreshToken := d.refreshTokenValue()
	if refreshToken == "" {
		return fmt.Errorf("refresh_token is empty: 没有 client_id/client_secret 时，"+
			"可在 %s 完成 OpenList 公共授权并把得到的 refresh_token 填进来",
			OpenListAuthorizeURL)
	}

	if apiURL := d.renewAPIURL(); apiURL != "" {
		return d.renewViaOnlineAPI(apiURL, refreshToken)
	}

	if d.Config.ClientID == "" {
		return fmt.Errorf("client_id is empty: either fill api_url (%s) to use the "+
			"OpenList public authorization, or provide client_id/client_secret",
			RenewAPIDefault)
	}
	if d.Config.ClientSecret == "" {
		return fmt.Errorf("client_secret is empty: either fill api_url (%s) to use the "+
			"OpenList public authorization, or provide client_id/client_secret",
			RenewAPIDefault)
	}

	logx.L().Infof("baidu netdisk refreshing token via official oauth")

	params := url.Values{}
	params.Set("grant_type", "refresh_token")
	params.Set("refresh_token", refreshToken)
	params.Set("client_id", d.Config.ClientID)
	params.Set("client_secret", d.Config.ClientSecret)

	reqURL := TokenURL + "?" + params.Encode()
	resp, err := d.client.Get(reqURL)
	if err != nil {
		return fmt.Errorf("refresh token request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read token response failed: %w", err)
	}

	var tokenResp TokenResp
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return fmt.Errorf("parse token response failed: %w", err)
	}

	if tokenResp.AccessToken == "" {
		var errResp TokenErrResp
		json.Unmarshal(body, &errResp)
		return fmt.Errorf("refresh token failed: %s %s", errResp.Error, errResp.ErrorDescription)
	}

	d.setTokens(tokenResp.AccessToken, tokenResp.RefreshToken)
	d.persistConfig()

	logx.L().Infof("baidu netdisk token refreshed successfully")
	return nil
}

// renewAPIURL 返回在线续期地址；未配置（或只有空白）时返回空串
func (d *BaiduNetdiskDriver) renewAPIURL() string {
	if d.Config == nil {
		return ""
	}
	return strings.TrimSpace(d.Config.APIURL)
}

// renewViaOnlineAPI 通过 OpenList 公共授权（oplist renewapi）在线续期
//
// 请求参数与 OpenList（drivers/baidu_netdisk/util.go:36）完全一致：
// refresh_ui=refresh_token、server_use=true、driver_txt=baiduyun_go。
// 返回体里的 refresh_token 是轮换后的新值，必须回写配置并持久化。
func (d *BaiduNetdiskDriver) renewViaOnlineAPI(apiURL, refreshUI string) error {
	logx.L().Infof("baidu netdisk refreshing token via openlist public api: %s", apiURL)

	req, err := http.NewRequest(http.MethodGet, apiURL, nil)
	if err != nil {
		return fmt.Errorf("build renew api request failed: %w", err)
	}
	q := req.URL.Query()
	q.Set("refresh_ui", refreshUI)
	q.Set("server_use", "true")
	q.Set("driver_txt", RenewDriverTxt)
	req.URL.RawQuery = q.Encode()

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("renew api request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read renew api response failed: %w", err)
	}

	var parsed RenewTokenResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("parse renew api response failed (status=%d): %w", resp.StatusCode, err)
	}
	if parsed.AccessToken == "" || parsed.RefreshToken == "" {
		if msg := parsed.ErrorMessage(); msg != "" {
			return fmt.Errorf("failed to refresh token: %s", msg)
		}
		return fmt.Errorf("empty access_token or refresh_token from renew api (status=%d)", resp.StatusCode)
	}

	if parsed.RefreshToken != refreshUI {
		logx.L().Infof("baidu netdisk refresh token rotated by renew api")
	}
	d.setTokens(parsed.AccessToken, parsed.RefreshToken)
	d.persistConfig()

	logx.L().Infof("baidu netdisk token refreshed successfully via openlist")
	return nil
}

// persistConfig 把刷新后的令牌写回配置并持久化到数据库
//
// 通过宿主注入的 persister 回写（驱动不直接依赖 db 层）；refresh_token 必须
// 回写轮换值，否则下一次续期仍旧用已作废的旧令牌。
func (d *BaiduNetdiskDriver) persistConfig() {
	d.Config.AccessToken = d.token()
	if refresh := d.refreshTokenValue(); refresh != "" {
		d.Config.RefreshToken = refresh
	}

	if d.configPersister == nil || d.providerID == 0 {
		return
	}
	configJSON, err := json.Marshal(d.Config)
	if err != nil {
		logx.L().Errorf("baidu netdisk marshal config failed: %v", err)
		return
	}
	if err := d.configPersister.UpdateConfig(d.providerID, string(configJSON)); err != nil {
		logx.L().Errorf("baidu netdisk persist refreshed token failed: %v", err)
	}
}

// request 发送 API 请求
func (d *BaiduNetdiskDriver) request(method, path string, params map[string]string, form url.Values) ([]byte, error) {
	return d.requestWith(method, path, params, form, true)
}

// requestWith 发送 API 请求；allowRefresh 为 false 时不再因 token 失效续期重试
//
// 续期后只重试一次：续期成功但服务端仍返回 111/-6 时，若无条件递归重试会
// 无限打续期接口（历史上的死循环形态）。
func (d *BaiduNetdiskDriver) requestWith(method, path string, params map[string]string, form url.Values, allowRefresh bool) ([]byte, error) {
	reqURL := APIBaseURL + path

	// 记下本次请求用的 token：续期去重时要靠它判断「是否已被别的协程换掉」
	token := d.token()

	// 构建查询参数
	query := url.Values{}
	query.Set("access_token", token)
	for k, v := range params {
		query.Set(k, v)
	}
	reqURL += "?" + query.Encode()

	var req *http.Request
	var err error

	if form != nil {
		req, err = http.NewRequest(method, reqURL, strings.NewReader(form.Encode()))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req, err = http.NewRequest(method, reqURL, nil)
		if err != nil {
			return nil, err
		}
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// 检查错误
	var errno struct {
		Errno int `json:"errno"`
	}
	json.Unmarshal(body, &errno)

	// Token 过期，自动刷新重试
	if errno.Errno == 111 || errno.Errno == -6 {
		if !allowRefresh {
			return body, fmt.Errorf("api error: errno=%d, url=%s (token still rejected after refresh)",
				errno.Errno, path)
		}
		if refreshErr := d.refreshIfStale(token); refreshErr != nil {
			return nil, fmt.Errorf("refresh token failed: %w", refreshErr)
		}
		return d.requestWith(method, path, params, form, false)
	}

	if errno.Errno != 0 {
		return body, fmt.Errorf("api error: errno=%d, url=%s", errno.Errno, path)
	}

	return body, nil
}

// get 发送 GET 请求
func (d *BaiduNetdiskDriver) get(path string, params map[string]string) ([]byte, error) {
	return d.request(http.MethodGet, path, params, nil)
}

// postForm 发送 POST 表单请求
func (d *BaiduNetdiskDriver) postForm(path string, params map[string]string, form url.Values) ([]byte, error) {
	return d.request(http.MethodPost, path, params, form)
}

// getUserInfo 获取用户信息
func (d *BaiduNetdiskDriver) getUserInfo() (*UserInfoResp, error) {
	params := map[string]string{
		"method": "uinfo",
	}
	body, err := d.get("/xpan/nas", params)
	if err != nil {
		return nil, err
	}

	var resp UserInfoResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// getQuota 获取配额信息
func (d *BaiduNetdiskDriver) getQuota() (*QuotaResp, error) {
	body, err := d.get("/xpan/nas", map[string]string{"method": "uinfo"})
	if err != nil {
		return nil, err
	}

	var resp QuotaResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// getFileList 获取文件列表
func (d *BaiduNetdiskDriver) getFileList(dir string) ([]File, error) {
	start := 0
	limit := 1000
	params := map[string]string{
		"method": "list",
		"dir":    dir,
		"web":    "1",
	}

	var allFiles []File
	for {
		params["start"] = strconv.Itoa(start)
		params["limit"] = strconv.Itoa(limit)

		body, err := d.get("/xpan/file", params)
		if err != nil {
			return nil, err
		}

		var resp ListResp
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, err
		}

		if len(resp.List) == 0 {
			break
		}

		allFiles = append(allFiles, resp.List...)

		if len(resp.List) < limit {
			break
		}
		start += limit
	}

	return allFiles, nil
}

// getDownloadLink 获取下载链接
func (d *BaiduNetdiskDriver) getDownloadLink(fsID int64) (string, error) {
	params := map[string]string{
		"method": "filemetas",
		"fsids":  fmt.Sprintf("[%d]", fsID),
		"dlink":  "1",
	}

	body, err := d.get("/xpan/multimedia", params)
	if err != nil {
		return "", err
	}

	var resp DownloadResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", err
	}

	if len(resp.List) == 0 {
		return "", fmt.Errorf("no download link found")
	}

	// 302 重定向获取真实下载链接
	dlink := resp.List[0].Dlink + "&access_token=" + d.accessToken
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, _ := http.NewRequest("HEAD", dlink, nil)
	req.Header.Set("User-Agent", "pan.baidu.com")

	resp2, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp2.Body.Close()

	location := resp2.Header.Get("Location")
	if location != "" {
		return location, nil
	}

	return dlink, nil
}

// precreate 预上传
func (d *BaiduNetdiskDriver) precreate(path string, size int64, blockList, contentMD5, sliceMD5 string) (*PrecreateResp, error) {
	form := url.Values{}
	form.Set("path", path)
	form.Set("size", strconv.FormatInt(size, 10))
	form.Set("isdir", "0")
	form.Set("autoinit", "1")
	form.Set("rtype", "3")
	form.Set("block_list", blockList)

	if contentMD5 != "" && sliceMD5 != "" {
		form.Set("content-md5", contentMD5)
		form.Set("slice-md5", sliceMD5)
	}

	params := map[string]string{
		"method": "precreate",
	}

	body, err := d.postForm("/xpan/file", params, form)
	if err != nil {
		return nil, err
	}

	var resp PrecreateResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}

	return &resp, nil
}

// create 创建文件
func (d *BaiduNetdiskDriver) create(path string, size int64, isdir int, uploadid, blockList string, mtime, ctime int64) (*CreateResp, error) {
	form := url.Values{}
	form.Set("path", path)
	form.Set("size", strconv.FormatInt(size, 10))
	form.Set("isdir", strconv.Itoa(isdir))
	form.Set("rtype", "3")

	if mtime != 0 && ctime != 0 {
		form.Set("local_mtime", strconv.FormatInt(mtime, 10))
		form.Set("local_ctime", strconv.FormatInt(ctime, 10))
	}

	if uploadid != "" {
		form.Set("uploadid", uploadid)
	}
	if blockList != "" {
		form.Set("block_list", blockList)
	}

	params := map[string]string{
		"method": "create",
	}

	body, err := d.postForm("/xpan/file", params, form)
	if err != nil {
		return nil, err
	}

	var resp CreateResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}

	return &resp, nil
}

// uploadSlice 上传分片
func (d *BaiduNetdiskDriver) uploadSlice(uploadURL string, params map[string]string, fileName string, data io.Reader, size int64) error {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	part, err := writer.CreateFormFile("file", fileName)
	if err != nil {
		return err
	}

	if _, err := io.Copy(part, data); err != nil {
		return err
	}

	writer.Close()

	// 构建请求 URL
	reqURL := uploadURL + UploadSlicePath
	query := url.Values{}
	for k, v := range params {
		query.Set(k, v)
	}
	reqURL += "?" + query.Encode()

	req, err := http.NewRequest(http.MethodPost, reqURL, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	// 使用驱动的客户端，但为上传设置更长的超时
	uploadClient := &http.Client{
		Timeout:   5 * time.Minute,
		Transport: d.client.Transport,
	}
	resp, err := uploadClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	respStr := string(respBody)

	// 检查 uploadid 是否过期
	lower := strings.ToLower(respStr)
	if strings.Contains(lower, "uploadid") &&
		(strings.Contains(lower, "invalid") || strings.Contains(lower, "expired") || strings.Contains(lower, "not found")) {
		return fmt.Errorf("uploadid expired")
	}

	// 检查错误
	var errno struct {
		ErrorCode int `json:"error_code"`
		Errno     int `json:"errno"`
	}
	json.Unmarshal(respBody, &errno)
	if errno.ErrorCode != 0 || errno.Errno != 0 {
		return fmt.Errorf("upload slice failed: %s", respStr)
	}

	return nil
}

// getUploadURL 获取上传域名
func (d *BaiduNetdiskDriver) getUploadURL(path, uploadID string) string {
	params := map[string]string{
		"method":         "locateupload",
		"appid":          "250528",
		"path":           path,
		"uploadid":       uploadID,
		"upload_version": "2.0",
	}

	body, err := d.get("/xpan/file", params)
	if err != nil {
		return UploadAPI
	}

	var resp UploadServerResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return UploadAPI
	}

	if len(resp.Servers) > 0 {
		return resp.Servers[0].Server
	}
	if len(resp.BakServers) > 0 {
		return resp.BakServers[0].Server
	}

	return UploadAPI
}

// mkdir 创建目录
func (d *BaiduNetdiskDriver) mkdir(path string) error {
	form := url.Values{}
	form.Set("path", path)
	form.Set("isdir", "1")
	form.Set("rtype", "3")

	params := map[string]string{
		"method": "create",
	}

	body, err := d.postForm("/xpan/file", params, form)
	if err != nil {
		// 如果目录已存在，忽略错误
		if strings.Contains(err.Error(), "errno=-8") {
			return nil
		}
		return err
	}

	var resp CreateResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return err
	}

	return nil
}

// manage 文件管理操作（移动、复制、删除、重命名）
//
// 返回 error 时通常附带百度原始响应 body（如 "errno=12 body=..."），便于调用方
// 识别内层 info[].errno=-7（路径无效）等"百度侧不可达"场景——这类场景下数据库
// 记录对应的物理副本本来就不存在或路径本身错误，调用方应视为"已清理"。
// 注意：网络层失败（postForm 出错且 body 为空）时 error 可能不含 body。
func (d *BaiduNetdiskDriver) manage(operation string, fileList interface{}) error {
	fileListJSON, err := json.Marshal(fileList)
	if err != nil {
		return err
	}

	form := url.Values{}
	form.Set("async", "0")
	form.Set("filelist", string(fileListJSON))
	form.Set("ondup", "fail")

	params := map[string]string{
		"method": "filemanager",
		"opera":  operation,
	}

	body, err := d.postForm("/xpan/file", params, form)
	if err != nil {
		// 即使返回错误也尽量附上 body，方便排查
		return fmt.Errorf("%w, body=%s", err, string(body))
	}
	// 解析顶层 errno + 内层 info[].errno
	var parsed struct {
		Errno int                      `json:"errno"`
		Info  []map[string]interface{} `json:"info"`
	}
	if jerr := json.Unmarshal(body, &parsed); jerr != nil {
		// body 非 JSON（HTML 错误页/网关 502 等）：请求结果未知，必须视为失败，
		// 绝不能当成功——否则上层会误删数据库行导致数据丢失
		return fmt.Errorf("manage %s: non-JSON response (len=%d), body=%s",
			operation, len(body), truncateBody(string(body), 512))
	}
	if parsed.Errno == 0 {
		return nil
	}
	return fmt.Errorf("errno=%d body=%s", parsed.Errno, string(body))
}

// truncateBody 截断过长 body 用于错误信息
func truncateBody(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

// getSliceSize 获取分片大小
func (d *BaiduNetdiskDriver) getSliceSize(fileSize int64) int64 {
	// 自定义分片大小
	if d.customPartSize > 0 {
		if d.customPartSize < DefaultSliceSize {
			return DefaultSliceSize
		}
		if d.vipType == 1 && d.customPartSize > VipSliceSize {
			return VipSliceSize
		}
		if d.vipType == 2 && d.customPartSize > SVipSliceSize {
			return SVipSliceSize
		}
		return d.customPartSize
	}

	// 根据会员类型自动选择
	switch d.vipType {
	case 2: // 超级会员
		return SVipSliceSize
	case 1: // 普通会员
		return VipSliceSize
	default: // 普通用户
		return DefaultSliceSize
	}
}
