package baidu

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jenken827/pfm-plugin-sdk/driver/types"
	"github.com/jenken827/pfm-plugin-sdk/plugin/sdk"
	"github.com/jenken827/pfm-plugin-sdk/plugin/sdk/logx"
	"github.com/jenken827/pfm-plugin-sdk/proto/gen/abi"
)

// errNoAuthentication 三种认证入口都没配
var errNoAuthentication = errors.New("no authentication configured: " +
	"need refresh_token (with api_url or client_id/client_secret) or access_token")

// DriverID 是本驱动的身份（落库在 storage_providers.provider_type）。
//
// 驱动身份由驱动自己声明：SDK 不认识任何具体驱动。
const DriverID = "baidu"

func init() {
	// 定义百度网盘驱动的元数据
	metadata := &abi.DriverInfo{
		DriverId: sdk.StringPtr(DriverID),
		Capabilities: []abi.DriverCapability{
			abi.DriverCapability_DRIVER_CAPABILITY_BATCH_DELETE,
			abi.DriverCapability_DRIVER_CAPABILITY_DIRECT_LINK,
			abi.DriverCapability_DRIVER_CAPABILITY_REMOTE_LIST,
		},
		Name:        sdk.StringPtr("BAIDU_NETDISK"),
		DisplayName: sdk.StringPtr("百度网盘"),
		Description: sdk.StringPtr("百度网盘存储提供者"),
		IconName:    sdk.StringPtr("cloud"),
		ConfigFields: []*abi.ConfigField{
			{
				Name:        sdk.StringPtr("refresh_token"),
				FieldType:   abi.ConfigFieldType_STRING.Enum(),
				Label:       sdk.StringPtr("Refresh Token"),
				Placeholder: sdk.StringPtr("百度网盘 OAuth2 Refresh Token"),
				HelperText: sdk.StringPtr("必填。可来自 OpenList 公共授权（https://api.oplist.org/）" +
					"或自有百度开放平台应用；用于换取和刷新 Access Token"),
				Required:  sdk.BoolPtr(true),
				IconName:  sdk.StringPtr("vpn_key"),
				InputType: sdk.StringPtr("password"),
			},
			{
				Name:        sdk.StringPtr("access_token"),
				FieldType:   abi.ConfigFieldType_STRING.Enum(),
				Label:       sdk.StringPtr("Access Token"),
				Placeholder: sdk.StringPtr("直接填写的 access_token"),
				HelperText: sdk.StringPtr("兜底选项（约 30 天有效期），一般不用填：" +
					"驱动会用上面的 Refresh Token 自动续期"),
				Required:  sdk.BoolPtr(false),
				IconName:  sdk.StringPtr("lock"),
				InputType: sdk.StringPtr("password"),
			},
			{
				Name:        sdk.StringPtr("api_url"),
				FieldType:   abi.ConfigFieldType_STRING.Enum(),
				Label:       sdk.StringPtr("续期服务地址"),
				Placeholder: sdk.StringPtr(RenewAPIDefault),
				HelperText: sdk.StringPtr("默认走 OpenList 公共授权，无需自有 Client ID/Secret；" +
					"使用自有应用时清空此项，改为填写下方 Client ID/Secret"),
				Required:     sdk.BoolPtr(false),
				DefaultValue: sdk.StringPtr(RenewAPIDefault),
				IconName:     sdk.StringPtr("settings_ethernet"),
			},
			{
				Name:        sdk.StringPtr("client_id"),
				FieldType:   abi.ConfigFieldType_STRING.Enum(),
				Label:       sdk.StringPtr("Client ID"),
				Placeholder: sdk.StringPtr("百度网盘应用 Client ID"),
				HelperText:  sdk.StringPtr("可选。填了续期服务地址时不需要；在百度开放平台创建应用后获取"),
				Required:    sdk.BoolPtr(false),
				IconName:    sdk.StringPtr("vpn_key"),
				InputType:   sdk.StringPtr("text"),
			},
			{
				Name:        sdk.StringPtr("client_secret"),
				FieldType:   abi.ConfigFieldType_STRING.Enum(),
				Label:       sdk.StringPtr("Client Secret"),
				Placeholder: sdk.StringPtr("百度网盘应用 Client Secret"),
				HelperText:  sdk.StringPtr("可选。仅在清空续期服务地址、用自有应用本地续期时需要"),
				Required:    sdk.BoolPtr(false),
				IconName:    sdk.StringPtr("lock"),
				InputType:   sdk.StringPtr("password"),
			},
			{
				Name:         sdk.StringPtr("root_path"),
				FieldType:    abi.ConfigFieldType_STRING.Enum(),
				Label:        sdk.StringPtr("根目录路径"),
				Placeholder:  sdk.StringPtr("/"),
				HelperText:   sdk.StringPtr("百度网盘中的根目录路径"),
				Required:     sdk.BoolPtr(false),
				DefaultValue: sdk.StringPtr("/"),
				IconName:     sdk.StringPtr("folder_open"),
			},
			{
				Name:         sdk.StringPtr("upload_thread"),
				FieldType:    abi.ConfigFieldType_NUMBER.Enum(),
				Label:        sdk.StringPtr("上传线程数"),
				Placeholder:  sdk.StringPtr("3"),
				HelperText:   sdk.StringPtr("并发上传线程数（1-32）"),
				Required:     sdk.BoolPtr(false),
				DefaultValue: sdk.StringPtr("3"),
				IconName:     sdk.StringPtr("speed"),
				InputType:    sdk.StringPtr("number"),
			},
			{
				Name:         sdk.StringPtr("custom_upload_part_size"),
				FieldType:    abi.ConfigFieldType_NUMBER.Enum(),
				Label:        sdk.StringPtr("自定义分片大小(MB)"),
				Placeholder:  sdk.StringPtr("0"),
				HelperText:   sdk.StringPtr("0为自动：普通用户4MB，会员16MB，超级会员32MB"),
				Required:     sdk.BoolPtr(false),
				DefaultValue: sdk.StringPtr("0"),
				IconName:     sdk.StringPtr("storage"),
				InputType:    sdk.StringPtr("number"),
			},
		},
	}

	types.RegisterStorageDriverWithMetadata(func(provider *types.Provider) (types.StorageDriver, error) {
		c := &Config{}
		if provider.ConfigJSON != "" {
			if err := json.Unmarshal([]byte(provider.ConfigJSON), c); err != nil {
				logx.L().Errorf("unmarshal baidu netdisk config failed: %v", err)
				return nil, err
			}
		}

		driver := &BaiduNetdiskDriver{
			Config:         c,
			providerID:     provider.ID,
			ProviderConfig: provider.Config,
			client: &http.Client{
				// 注意：不能设置 Timeout（http.Client.Timeout 是整个请求的绝对超时，
				// 数据传输中也会触发）。百度网盘对非会员下载限速，大文件预览/下载时
				// 30s 绝对超时会强断仍在传输的连接 → 前端 "Connection closed while
				// receiving data"（加载到一半报错）。
				// 改用 ResponseHeaderTimeout 只约束响应头等待；body 读取无绝对超时，
				// 由请求 ctx（前端断开时取消）控制生命周期。
				Transport: &http.Transport{
					MaxIdleConns:          20,
					MaxIdleConnsPerHost:   10,
					MaxConnsPerHost:       15,
					IdleConnTimeout:       90 * time.Second,
					TLSHandshakeTimeout:   10 * time.Second,
					ResponseHeaderTimeout: 30 * time.Second,
					DisableKeepAlives:     false,
				},
			},
		}

		// 认证方式预检：缺失只告警，不阻断初始化——与前身实现一致，
		// 真正报错推迟到第一次请求（配置页允许先落库后补 token）。
		if err := checkAuthentication(c); err != nil {
			logx.L().Warnf("baidu netdisk authentication incomplete for provider %d: %v",
				provider.ID, err)
		}

		driver.setTokens(c.AccessToken, c.RefreshToken)
		driver.customPartSize = c.CustomUploadPartSize.Int() * 1024 * 1024 // MB to bytes

		// 没有 access_token 时先续期一次（续期失败不阻断，首次请求会再试）
		if driver.token() == "" && c.RefreshToken != "" {
			if err := driver.doRefreshToken(); err != nil {
				logx.L().Warnf("refresh baidu netdisk token failed: %v", err)
			}
		}

		// 获取用户信息（验证 token 有效性）
		if driver.token() != "" {
			userInfo, err := driver.getUserInfo()
			if err != nil {
				logx.L().Warnf("get baidu netdisk user info failed: %v", err)
			} else {
				driver.vipType = userInfo.VipType
				logx.L().Infof("baidu netdisk user: %s, vip_type: %d", userInfo.BaiduName, userInfo.VipType)
			}
		}

		// 解析上传线程数
		uploadThread := 3
		if t := int(c.UploadThread.Int()); t >= 1 && t <= 32 {
			uploadThread = t
		}
		_ = uploadThread // 暂时未使用，后续可支持并发上传

		return driver, nil
	}, metadata)
}

// checkAuthentication 校验是否至少配置了一种可用的认证方式。
//
// 三种：手填 access_token / OpenList 公共授权（refresh_token + api_url）/
// 自有应用（refresh_token + client_id + client_secret）。
func checkAuthentication(c *Config) error {
	if c == nil {
		return errNoAuthentication
	}
	if c.AccessToken != "" {
		return nil
	}
	if c.RefreshToken == "" {
		return errNoAuthentication
	}
	if c.APIURL != "" || (c.ClientID != "" && c.ClientSecret != "") {
		return nil
	}
	return errors.New("refresh_token is set but neither api_url nor client_id/client_secret: " +
		"fill api_url with the OpenList public authorization endpoint (" + RenewAPIDefault + ") " +
		"or provide your own app credentials")
}
