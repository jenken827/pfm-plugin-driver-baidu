package baidu

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jenken827/pfm-plugin-sdk/driver/types"
)

// 编译期断言：驱动改签名时在这里直接失败，而不是静默失去直链能力
var _ types.DirectLinker = (*BaiduNetdiskDriver)(nil)

// DirectLink 实现 types.DirectLinker：返回百度网盘的下载直链
//
// 与 OpenChunk 共用 getDownloadLink（内部已 HEAD 跟随 302 拿到真实地址）。
// 真实地址必须带 User-Agent: pan.baidu.com 才能下载（与 OpenChunk 一致），
// 所以浏览器无法直接使用；有效期按保守 1 小时处理。
func (d *BaiduNetdiskDriver) DirectLink(ctx context.Context, ref types.FileRef) (*types.DirectLink, error) {
	if ref.ProviderFileID == "" {
		return nil, fmt.Errorf("provider_file_id is empty")
	}

	fsID, err := strconv.ParseInt(ref.ProviderFileID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid fs_id %q: %w", ref.ProviderFileID, err)
	}

	url, err := d.getDownloadLink(fsID)
	if err != nil {
		return nil, err
	}

	return &types.DirectLink{
		URL:           url,
		Headers:       map[string]string{"User-Agent": "pan.baidu.com"},
		ExpiresAt:     time.Now().Add(time.Hour),
		SupportsRange: true,
	}, nil
}
