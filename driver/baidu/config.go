package baidu

import (
	"github.com/jenken827/pfm-plugin-sdk/plugin/sdk"
)

// Config 百度网盘驱动的配置
//
// 为什么定义在插件里，而不是用宿主 proto 的消息：
//
// 驱动已经迁出为独立插件进程，宿主只把 storage_providers.config 当作**不透明
// JSON 字符串**存取（加解密、备份、迁移都不解析它），配置结构因此是驱动私有的
// 数据定义，应该由驱动自己拥有、自己演进。插件再反向依赖宿主的 driver.proto，
// 会让「改驱动配置」必须先改宿主仓库并重新生成 proto，宿主也白白背着一堆它
// 从不使用的消息。
//
// JSON 键名必须**逐字**等于元数据里 ConfigField.Name（即 init.go 的 name 字段）：
// 前端表单按字段名拼 JSON，宿主原样透传，驱动按下面的 tag 解析。
type Config struct {
	// RefreshToken 续期令牌。两条续期路径都用它：
	// OpenList 公共授权（作为 refresh_ui）或百度官方 OAuth2（作为 refresh_token）
	RefreshToken string `json:"refresh_token,omitempty"`
	// AccessToken 兜底的手填令牌：非空时优先使用，失效后由续期路径接管
	AccessToken string `json:"access_token,omitempty"`
	// RootPath 网盘内的根目录（默认 "/"）
	RootPath string `json:"root_path,omitempty"`
	// UploadThread 分片上传并发数（1-32）
	UploadThread sdk.FormNum `json:"upload_thread,omitempty"`
	// CustomUploadPartSize 自定义分片大小（MB，0 表示按会员等级自动选择）
	CustomUploadPartSize sdk.FormNum `json:"custom_upload_part_size,omitempty"`
	// ClientID / ClientSecret 百度开放平台应用凭据：仅在本地续期时使用，
	// api_url 非空（走 OpenList 公共授权）时不需要
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	// APIURL 在线续期服务地址。非空则走 OpenList 公共授权（默认
	// https://api.oplist.org/baiduyun/renewapi），此时无需自有 client 凭据；
	// 留空则用 client_id/client_secret 调百度官方 OAuth2 本地续期
	APIURL string `json:"api_url,omitempty"`
}
