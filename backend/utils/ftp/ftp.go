package ftp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"oneimg/backend/utils/storageobject"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/jlaffaye/ftp"
)

// FTP keys are relative to the login directory in every object lifecycle operation.
func ObjectPath(key string) string {
	return path.Clean(strings.TrimLeft(strings.ReplaceAll(key, "\\", "/"), "/"))
}

// FTPConfig FTP 配置结构体
type FTPConfig struct {
	Context  context.Context // Optional request lifetime for control and data connections.
	Host     string          // FTP服务器地址（如 192.168.1.100）
	Port     int             // FTP端口（默认21）
	User     string          // 用户名
	Password string          // 密码
	Timeout  int             // 连接超时时间（秒，默认5）
	// 新版库默认启用被动模式，无需手动配置
}

// FTP has no atomic create-if-absent. Fail closed when listing cannot establish absence.
func (f *FTPUtil) UploadCreate(ctx context.Context, path string, body io.Reader) error {
	f.config.Context = ctx
	client, err := f.GetClient()
	if err != nil {
		return err
	}
	path = ObjectPath(path)
	dir := filepath.Dir(path)
	if err := f.makeDirRecursive(client, dir); err != nil {
		return err
	}
	entries, err := client.List(dir)
	if err != nil {
		return errors.New("cannot establish FTP object absence")
	}
	for _, entry := range entries {
		if entry.Name == filepath.Base(path) {
			return storageobject.ErrConflict
		}
	}
	if err := client.Stor(path, body); err != nil {
		return storageobject.ErrOutcomeUncertain
	}
	return nil
}

// FTPUtil FTP工具类
type FTPUtil struct {
	config FTPConfig
	conn   *ftp.ServerConn // FTP连接实例
}

// NewFTPUtil 初始化FTP工具类
func NewFTPUtil(config FTPConfig) *FTPUtil {
	// 设置默认值
	if config.Port == 0 {
		config.Port = 21
	}
	if config.Timeout == 0 {
		config.Timeout = 5
	}

	return &FTPUtil{
		config: config,
	}
}

// GetClient 获取FTP客户端连接（复用已有连接，断开则重连）
func (f *FTPUtil) GetClient() (*ftp.ServerConn, error) {
	// 检查现有连接是否有效
	if f.conn != nil {
		// 发送NOOP命令检测连接是否存活
		if err := f.conn.NoOp(); err == nil {
			return f.conn, nil
		}
		// 连接失效，关闭旧连接
		_ = f.conn.Quit()
		f.conn = nil
	}

	// 构建连接地址
	addr := fmt.Sprintf("%s:%d", f.config.Host, f.config.Port)

	// 正确的连接创建方式（新版库）
	requestCtx := f.config.Context
	if requestCtx == nil {
		requestCtx = context.Background()
	}
	dialer := &net.Dialer{Timeout: time.Duration(f.config.Timeout) * time.Second}
	conn, err := ftp.Dial(addr,
		ftp.DialWithTimeout(time.Duration(f.config.Timeout)*time.Second),
		ftp.DialWithDialFunc(func(network, address string) (net.Conn, error) {
			connection, err := dialer.DialContext(requestCtx, network, address)
			if err != nil {
				return nil, err
			}
			stop := context.AfterFunc(requestCtx, func() { _ = connection.Close() })
			return &requestConn{Conn: connection, stop: stop}, nil
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("FTP连接失败: %w", err)
	}

	// 登录
	if err := conn.Login(f.config.User, f.config.Password); err != nil {
		_ = conn.Quit()
		return nil, fmt.Errorf("FTP登录失败: %w", err)
	}

	// 🔥 新版库无需手动调用 EnterPassiveMode()，默认启用被动模式
	// 如需强制主动模式（极少场景），可使用：
	// conn.SetTransferMode(ftp.Active)

	f.conn = conn
	return conn, nil
}

// UploadImage 上传图片到FTP服务器
// remotePath: 远程文件路径（如 /uploads/2025/01/test.webp）
// imgBytes: 图片字节流
// contentType: 图片MIME类型（可选，仅日志用）
func (f *FTPUtil) UploadImage(remotePath string, imgBytes []byte, contentType string) error {
	remotePath = ObjectPath(remotePath)
	// 获取客户端
	client, err := f.GetClient()
	if err != nil {
		return err
	}

	// 递归创建远程目录
	remoteDir := filepath.Dir(remotePath)
	if err := f.makeDirRecursive(client, remoteDir); err != nil {
		return fmt.Errorf("创建远程目录失败: %w", err)
	}

	// 上传文件
	reader := bytes.NewReader(imgBytes)
	if err := client.Stor(remotePath, reader); err != nil {
		return fmt.Errorf("上传图片失败: %w", err)
	}

	return nil
}

// DeleteImage 删除FTP服务器上的图片
// remotePath: 远程文件路径
func (f *FTPUtil) DeleteImage(remotePath string) error {
	remotePath = ObjectPath(remotePath)
	// 获取客户端
	client, err := f.GetClient()
	if err != nil {
		return err
	}

	// 删除文件
	if err := client.Delete(remotePath); err != nil {
		// 兼容不同FTP服务器的错误码（文件不存在）
		if strings.Contains(err.Error(), "550") || strings.Contains(err.Error(), "No such file") {
			return errors.New("文件不存在")
		}
		return fmt.Errorf("删除图片失败: %w", err)
	}

	return nil
}

// Close 关闭FTP连接
func (f *FTPUtil) Close() error {
	if f.conn != nil {
		err := f.conn.Quit()
		f.conn = nil
		return err
	}
	return nil
}

// makeDirRecursive 递归创建FTP目录
func (f *FTPUtil) makeDirRecursive(client *ftp.ServerConn, dir string) error {
	// 处理空目录/根目录
	if dir == "/" || dir == "." || dir == "" {
		return nil
	}

	// 修复：强制替换所有反斜杠为正斜杠
	dir = strings.ReplaceAll(dir, "\\", "/")
	// 拆分目录层级（仅按/拆分）
	dirs := strings.Split(strings.Trim(dir, "/"), "/")
	currentPath := ""

	for _, d := range dirs {
		if d == "" {
			continue
		}

		// 拼接当前层级（始终用/）
		if currentPath == "" {
			currentPath = d
		} else {
			currentPath = fmt.Sprintf("%s/%s", currentPath, d)
		}

		// 逐层级创建（关键：避免一次性创建多级）
		err := client.MakeDir(currentPath)
		// 兼容错误：目录已存在/权限提示
		if err != nil {
			errMsg := strings.ToLower(err.Error())
			if strings.Contains(errMsg, "550") || strings.Contains(errMsg, "already exists") {
				continue // 忽略已存在
			}
			if strings.Contains(errMsg, "553") {
				return fmt.Errorf("目录名被服务器禁止（%s）：%w", currentPath, err)
			}
			return fmt.Errorf("创建目录 %s 失败: %w", currentPath, err)
		}
	}

	return nil
}

// requestConn stops its cancellation callback when the FTP library closes a connection.
type requestConn struct {
	net.Conn
	stop func() bool
}

func (c *requestConn) Close() error { c.stop(); return c.Conn.Close() }
