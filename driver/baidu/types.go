package baidu

// TokenErrResp Token 错误响应
type TokenErrResp struct {
	ErrorDescription string `json:"error_description"`
	Error            string `json:"error"`
}

// TokenResp Token 响应
type TokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

// RenewTokenResp oplist renewapi 在线续期响应
//
// 与 OpenList（drivers/baidu_netdisk/util.go）解析的字段一致；错误时服务端
// 可能只回 text（例如 "refresh token is invalid"），故把几种错误字段都留下。
type RenewTokenResp struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int64  `json:"expires_in"`
	Text             string `json:"text"`
	Message          string `json:"message"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// ErrorMessage 返回响应里第一个非空的错误描述
func (r RenewTokenResp) ErrorMessage() string {
	for _, s := range []string{r.Text, r.ErrorDescription, r.Message, r.Error} {
		if s != "" {
			return s
		}
	}
	return ""
}

// File 百度网盘文件信息
type File struct {
	Category int   `json:"category"`
	FsId     int64 `json:"fs_id"`
	Thumbs   struct {
		Url3 string `json:"url3"`
	} `json:"thumbs"`
	Size           int64  `json:"size"`
	Path           string `json:"path"`
	ServerFilename string `json:"server_filename"`
	Md5            string `json:"md5"`
	Isdir          int    `json:"isdir"`

	// list resp
	ServerCtime int64 `json:"server_ctime"`
	ServerMtime int64 `json:"server_mtime"`
	LocalMtime  int64 `json:"local_mtime"`
	LocalCtime  int64 `json:"local_ctime"`

	// only create and precreate resp
	Ctime int64 `json:"ctime"`
	Mtime int64 `json:"mtime"`
}

// ListResp 文件列表响应
type ListResp struct {
	Errno    int    `json:"errno"`
	GuidInfo string `json:"guid_info"`
	List     []File `json:"list"`
	Guid     int    `json:"guid"`
}

// DownloadResp 下载响应
type DownloadResp struct {
	Errmsg string `json:"errmsg"`
	Errno  int    `json:"errno"`
	List   []struct {
		Dlink string `json:"dlink"`
	} `json:"list"`
	RequestId string `json:"request_id"`
}

// PrecreateResp 预上传响应
type PrecreateResp struct {
	Errno      int `json:"errno"`
	ReturnType int `json:"return_type"`

	// return_type=1
	Path      string `json:"path"`
	Uploadid  string `json:"uploadid"`
	BlockList []int  `json:"block_list"`

	// return_type=2
	File File `json:"info"`

	// 保存断点续传对应的上传域名（不序列化）
	UploadURL string `json:"-"`
}

// CreateResp 创建文件响应
type CreateResp struct {
	Errno int    `json:"errno"`
	Path  string `json:"path"`
	FsId  int64  `json:"fs_id"`
	Ctime int64  `json:"ctime"`
	Mtime int64  `json:"mtime"`
}

// UploadServerResp 上传服务器响应
type UploadServerResp struct {
	ErrorCode int    `json:"error_code"`
	ErrorMsg  string `json:"error_msg"`
	Servers   []struct {
		Server string `json:"server"`
	} `json:"servers"`
	BakServers []struct {
		Server string `json:"server"`
	} `json:"bak_servers"`
}

// QuotaResp 配额响应
type QuotaResp struct {
	Errno     int    `json:"errno"`
	RequestId string `json:"request_id"`
	Total     int64  `json:"total"`
	Used      int64  `json:"used"`
}

// UserInfoResp 用户信息响应
type UserInfoResp struct {
	Errno     int    `json:"errno"`
	RequestId string `json:"request_id"`
	VipType   int    `json:"vip_type"`
	BaiduName string `json:"baidu_name"`
	Uk        int64  `json:"uk"`
}

// ManageResp 文件管理响应（移动、复制、删除、重命名）
type ManageResp struct {
	Errno     int   `json:"errno"`
	RequestId int64 `json:"request_id"`
	Info      []struct {
		Path  string `json:"path"`
		Errno int    `json:"errno"`
	} `json:"info"`
}

const (
	// 上传相关
	UploadAPI        = "https://d.pcs.baidu.com"
	UploadSlicePath  = "/rest/2.0/pcs/superfile2"
	MaxSliceNum      = 2048
	DefaultSliceSize = 4 * 1024 * 1024  // 4MB
	VipSliceSize     = 16 * 1024 * 1024 // 16MB
	SVipSliceSize    = 32 * 1024 * 1024 // 32MB

	// RenewDriverTxt 在线续期时告知 oplist 调用方是哪个驱动实现，
	// 取值与 OpenList（drivers/baidu_netdisk/util.go）保持一致
	RenewDriverTxt = "baiduyun_go"
)

// OpenListAuthorizeURL OpenList 公共授权系统首页（没有自有 client_id/client_secret 时用它）
const OpenListAuthorizeURL = "https://api.oplist.org/"

// 服务端点。声明为变量（而非常量）只为让测试能把请求指向本地假服务器。
var (
	// APIBaseURL 百度网盘 API 基础地址
	APIBaseURL = "https://pan.baidu.com/rest/2.0"

	// TokenURL 百度官方 OAuth2 Token 刷新地址（自有 client 凭据时使用）
	TokenURL = "https://openapi.baidu.com/oauth/2.0/token"

	// RenewAPIDefault OpenList 公共授权（oplist）在线续期地址：
	// 只需 refresh_token（refresh_ui），无需自有 client_id/client_secret
	RenewAPIDefault = "https://api.oplist.org/baiduyun/renewapi"
)
