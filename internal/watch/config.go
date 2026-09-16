package watch

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

type Profile struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Username    string `json:"username"`
	Password    string `json:"password,omitempty"`
	HasPassword bool   `json:"hasPassword,omitempty"`
}
type Machine struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Host      string   `json:"host"`
	Port      int      `json:"port"`
	Group     string   `json:"group"`
	ProfileID string   `json:"profileId"`
	Commands  []string `json:"commands"`
	Enabled   bool     `json:"enabled"`
}
type Config struct {
	Interval int       `json:"interval"`
	Profiles []Profile `json:"profiles"`
	Machines []Machine `json:"machines"`
}
type Command struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Shell string `json:"shell"`
}

var Commands = []Command{
	{"npu", "NPU 状态", "npu-smi info"},
	{"python", "Python 进程", "ps -ef | awk 'NR == 1 || /[pP][yY][tT][hH][oO][nN]/'"},
	{"usage", "Python CPU / 内存", "ps -eo user,pid,ppid,pcpu,pmem,rss,etime,args --sort=-pcpu | awk 'NR == 1 || /[p]ython/'"},
}

func commandByID(id string) (Command, bool) {
	for _, c := range Commands {
		if c.ID == id {
			return c, true
		}
	}
	return Command{}, false
}

var identifier = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,80}$`)
var hostname = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]{0,252}$`)

func (c Config) Validate() error {
	if c.Interval < 3 || c.Interval > 3600 {
		return errors.New("刷新间隔需为 3–3600 秒")
	}
	if len(c.Profiles) > 100 || len(c.Machines) > 200 {
		return errors.New("最多支持 100 组凭据、200 台机器")
	}
	profiles := map[string]bool{}
	for _, p := range c.Profiles {
		if !identifier.MatchString(p.ID) || profiles[p.ID] {
			return errors.New("凭据 ID 无效或重复")
		}
		if strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.Username) == "" || len(p.Username) > 128 || len(p.Password) > 4096 {
			return errors.New("请填写凭据名称和 SSH 用户名，检查长度")
		}
		profiles[p.ID] = true
	}
	ids := map[string]bool{}
	for _, m := range c.Machines {
		if !identifier.MatchString(m.ID) || ids[m.ID] {
			return errors.New("机器 ID 无效或重复")
		}
		ids[m.ID] = true
		if strings.TrimSpace(m.Name) == "" || len(m.Name) > 160 || len(m.Group) > 80 {
			return errors.New("请填写机器名称，名称最长 160 字符，分组最长 80 字符")
		}
		if net.ParseIP(m.Host) == nil && !hostname.MatchString(m.Host) {
			return fmt.Errorf("%s：请输入 IP 或主机名，不包含协议、端口或空格", m.Name)
		}
		if m.Port < 1 || m.Port > 65535 {
			return errors.New("SSH 端口需为 1–65535")
		}
		if !profiles[m.ProfileID] {
			return fmt.Errorf("%s：请选择有效的共享凭据", m.Name)
		}
		if len(m.Commands) == 0 || len(m.Commands) > len(Commands) {
			return errors.New("每台机器至少选择一个监控命令")
		}
		seen := map[string]bool{}
		for _, id := range m.Commands {
			if _, ok := commandByID(id); !ok || seen[id] {
				return errors.New("监控命令无效或重复")
			}
			seen[id] = true
		}
	}
	return nil
}

type Store struct {
	mu     sync.Mutex
	dir    string
	key    []byte
	config Config
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, config: Config{Interval: 5, Profiles: []Profile{}, Machines: []Machine{}}}
	keyPath := filepath.Join(dir, "vault.key")
	key, err := os.ReadFile(keyPath)
	if os.IsNotExist(err) {
		// Never replace a lost key when an encrypted configuration already exists.
		if _, e := os.Stat(filepath.Join(dir, "config.enc")); !os.IsNotExist(e) {
			return nil, errors.New("配置存在但 vault.key 丢失，请从备份同时恢复两个文件")
		}
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		if err = writeAtomic(keyPath, key); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("vault.key 无效")
	}
	s.key = key
	data, err := os.ReadFile(filepath.Join(dir, "config.enc"))
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	plain, err := s.decrypt(data)
	if err != nil {
		return nil, errors.New("无法解密配置，请确认 config.enc 与 vault.key 来自同一份备份")
	}
	if err = json.Unmarshal(plain, &s.config); err != nil {
		return nil, err
	}
	if err = s.config.Validate(); err != nil {
		return nil, err
	}
	return s, nil
}
func (s *Store) encrypt(data []byte) ([]byte, error) {
	b, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(b)
	if err != nil {
		return nil, err
	}
	n := make([]byte, g.NonceSize())
	if _, err = rand.Read(n); err != nil {
		return nil, err
	}
	return g.Seal(n, n, data, []byte("simpleHtmlWatch/v1")), nil
}
func (s *Store) decrypt(data []byte) ([]byte, error) {
	b, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(b)
	if err != nil {
		return nil, err
	}
	if len(data) < g.NonceSize() {
		return nil, errors.New("配置损坏")
	}
	return g.Open(nil, data[:g.NonceSize()], data[g.NonceSize():], []byte("simpleHtmlWatch/v1"))
}
func clone(c Config) Config {
	b, _ := json.Marshal(c)
	var v Config
	_ = json.Unmarshal(b, &v)
	return v
}
func (s *Store) Snapshot() Config { s.mu.Lock(); defer s.mu.Unlock(); return clone(s.config) }
func (s *Store) Public() Config {
	c := s.Snapshot()
	for i := range c.Profiles {
		c.Profiles[i].HasPassword = c.Profiles[i].Password != ""
		c.Profiles[i].Password = ""
	}
	return c
}
func (s *Store) Save(c Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := map[string]string{}
	for _, p := range s.config.Profiles {
		old[p.ID] = p.Password
	}
	for i := range c.Profiles {
		if c.Profiles[i].Password == "" {
			c.Profiles[i].Password = old[c.Profiles[i].ID]
		}
		c.Profiles[i].HasPassword = false
		if c.Profiles[i].Password == "" {
			return fmt.Errorf("%s：请输入 SSH 密码", c.Profiles[i].Name)
		}
	}
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	enc, err := s.encrypt(b)
	if err != nil {
		return err
	}
	if err = writeAtomic(filepath.Join(s.dir, "config.enc"), enc); err != nil {
		return err
	}
	s.config = clone(c)
	return nil
}
func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".watch-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}
func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
