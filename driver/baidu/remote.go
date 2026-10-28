package baidu

import (
	"context"
	"path"
	"strconv"

	"github.com/jenken827/pfm-plugin-sdk/driver/types"
)

var _ types.RemoteLister = (*BaiduNetdiskDriver)(nil)

// RemoteRoot 实现 types.RemoteLister：百度 provider 配置的根目录（始终带前导 "/"）
func (d *BaiduNetdiskDriver) RemoteRoot() string {
	return d.rootPath()
}

// ListRemote 实现 types.RemoteLister：列出百度网盘目录下的直接子项。
// 百度 list 返回的 path 即完整路径（file_locations.path 用的也是它），优先直接采用。
func (d *BaiduNetdiskDriver) ListRemote(ctx context.Context, dir string) ([]types.RemoteEntry, error) {
	files, err := d.getFileList(dir)
	if err != nil {
		return nil, err
	}
	out := make([]types.RemoteEntry, 0, len(files))
	for _, f := range files {
		full := f.Path
		if full == "" {
			full = path.Join(dir, f.ServerFilename)
		}
		out = append(out, types.RemoteEntry{
			Path:  full,
			Name:  f.ServerFilename,
			ID:    strconv.FormatInt(f.FsId, 10),
			IsDir: f.Isdir == 1,
			Size:  f.Size,
		})
	}
	return out, nil
}
