package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "embed"
)

//go:embed zh.json.gz
var zhGz []byte

const (
	targetEntry = "i18n/ja-JP.json"
	toolName    = "Claude 桌面版一键汉化"
	toolVer     = "1.3.0"
)

// logEmit 由界面层接管，用于把日志实时推到窗口里；为空时直接打印到控制台
var logEmit func(string)

func logf(format string, a ...any) {
	s := fmt.Sprintf(format, a...)
	if logEmit != nil {
		logEmit(s)
		return
	}
	fmt.Print(s)
}

func init() {
	k := syscall.NewLazyDLL("kernel32.dll")
	k.NewProc("SetConsoleOutputCP").Call(65001)
}

type Integrity struct {
	Algorithm string   `json:"algorithm"`
	Hash      string   `json:"hash"`
	BlockSize int64    `json:"blockSize"`
	Blocks    []string `json:"blocks"`
}

type Entry struct {
	Size      int64             `json:"size"`
	Offset    string            `json:"offset"`
	Integrity *Integrity        `json:"integrity"`
	Files     map[string]*Entry `json:"files"`
}

// State 记录上次汉化时的文件指纹，用来判断 Claude 是否升级过
type State struct {
	ToolVersion string `json:"toolVersion"`
	PatchedAt   string `json:"patchedAt"`
	InstallDir  string `json:"installDir"`
	Version     string `json:"version"`
	HeaderHash  string `json:"asarHeaderHash"`
	AsarSize    int64  `json:"asarSize"`
	AsarMTime   int64  `json:"asarMTimeUnix"`
	Entries     int    `json:"entries"`
	Hit         int    `json:"hit"`
}

type AsarInfo struct {
	Buf       []byte
	Path      string
	HeaderRaw string
	HeaderEnd int64
	DataStart int64
	Entry     *Entry
	Abs       int64
	Content   []byte
	HeaderSHA string
}

// asarTarget 描述一个资源包里要汉化的语言表
type asarTarget struct {
	Asar    string
	Entries []string
}

// patchTargets Claude 6 起界面文案分散在两张 asar、四个语言表里。
// ion-dist.asar 是主界面，app.asar 管主进程菜单与窗口壳层。
var patchTargets = []asarTarget{
	{"ion-dist.asar", []string{"i18n/ja-JP.json", "i18n/dynamic/ja-JP.json"}},
	{"app.asar", []string{
		"resources/i18n/ja-JP.json",
		"resources/frame/shell/i18n/frame-shell/ja-JP.json",
	}},
}

type Analysis struct {
	Dir     string
	Version string
	Total   int
	Chinese int
	Ratio   float64
	Info    *AsarInfo
	State   *State
	Verdict string // already / partial / stale / upgrade / never
}

// Options 一次操作的全部参数
type Options struct {
	Dir      string
	Restore  bool
	Dry      bool
	Force    bool
	NoBackup bool
	Status   bool
	Quiet    bool
}

func main() {
	var (
		dir      = flag.String("dir", "", "指定 Claude 安装目录")
		restore  = flag.Bool("restore", false, "从备份还原成原版")
		dry      = flag.Bool("dry", false, "只检查不写入")
		list     = flag.Bool("list", false, "列出检测到的 Claude 安装")
		status   = flag.Bool("status", false, "只看状态诊断")
		force    = flag.Bool("force", false, "强制重新汉化")
		noBackup = flag.Bool("no-backup", false, "不创建备份")
		cli      = flag.Bool("cli", false, "强制使用命令行模式")
	)
	flag.Usage = func() {
		fmt.Printf("%s v%s\n\n", toolName, toolVer)
		fmt.Println("双击直接打开图形界面。命令行参数：")
		fmt.Println("  -dir <路径>   指定安装目录")
		fmt.Println("  -restore      还原成原版")
		fmt.Println("  -status       只看状态诊断")
		fmt.Println("  -dry          只检查不写入")
		fmt.Println("  -force        强制重新汉化")
		fmt.Println("  -list         列出安装")
		fmt.Println("  -no-backup    不备份")
		fmt.Println("  -cli          命令行模式（不弹窗）")
	}
	flag.Parse()

	// 没有任何参数、也没显式要求命令行 -> 开图形界面
	if flag.NFlag() == 0 && !*cli {
		runGUI()
		return
	}

	allocConsole()
	opt := Options{
		Dir: *dir, Restore: *restore, Dry: *dry,
		Force: *force, NoBackup: *noBackup, Status: *status,
	}
	if *list {
		dirs := dirsOf(opt)
		fmt.Println("检测到的安装：")
		for i, d := range dirs {
			fmt.Printf("  [%d] %s\n", i+1, d)
		}
		return
	}
	if err := runOnce(opt); err != nil {
		fmt.Printf("\n[错误] %v\n", err)
		os.Exit(1)
	}
}

func allocConsole() {
	k := syscall.NewLazyDLL("kernel32.dll")
	r, _, _ := k.NewProc("AttachConsole").Call(^uintptr(0))
	if r == 0 {
		k.NewProc("AllocConsole").Call()
	}
}

func dirsOf(opt Options) []string {
	if opt.Dir != "" {
		return []string{opt.Dir}
	}
	return detectDirs()
}

// runOnce 执行一次完整流程，所有输出都走 logf
func runOnce(opt Options) error {
	logf("== %s v%s ==\n\n", toolName, toolVer)

	dirs := dirsOf(opt)
	if len(dirs) == 0 {
		return errors.New("没有找到 Claude 桌面版安装目录。\n默认位置: %LOCALAPPDATA%\\AnthropicClaude\\app-<版本>\\")
	}

	st := loadState()
	warnNewerVersion(dirs, st)

	if opt.Restore {
		for _, d := range dirs {
			if err := doRestore(d); err != nil {
				logf("[失败] %s: %v\n", d, err)
			}
		}
		return nil
	}

	analyses := make([]*Analysis, 0, len(dirs))
	for _, d := range dirs {
		a, err := analyze(d, st)
		if err != nil {
			logf("目标: %s\n  [跳过] %v\n\n", d, err)
			continue
		}
		analyses = append(analyses, a)
	}
	if len(analyses) == 0 {
		return errors.New("所有安装都无法解析，请确认 Claude 版本")
	}

	if opt.Status {
		for _, a := range analyses {
			printAnalysis(a)
		}
		printStateInfo(st)
		return nil
	}

	didWork := false
	for _, a := range analyses {
		printAnalysis(a)
		if a.Verdict == "already" && !opt.Force && !opt.Dry {
			logf("  -> 已是中文，跳过（要重打请勾选「强制」）\n\n")
			continue
		}
		if err := doPatch(a, opt.Dry, opt.NoBackup); err != nil {
			logf("  [失败] %v\n\n", err)
			continue
		}
		didWork = true
		logf("\n")
	}
	if !didWork && !opt.Dry {
		logf("没有需要处理的安装。\n")
	}
	return nil
}

func detectDirs() []string {
	base := filepath.Join(os.Getenv("LOCALAPPDATA"), "AnthropicClaude")
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "app-") {
			continue
		}
		d := filepath.Join(base, e.Name())
		if _, err := os.Stat(filepath.Join(d, "resources", "ion-dist.asar")); err == nil {
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}

func asarPath(d string) string { return filepath.Join(d, "resources", "ion-dist.asar") }
func exePath(d string) string  { return filepath.Join(d, "claude.exe") }

func statePath() string {
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "AnthropicClaude", "claude-hanhua.state.json")
}

func loadState() *State {
	b, err := os.ReadFile(statePath())
	if err != nil {
		return nil
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil
	}
	return &s
}

func saveState(s *State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(statePath(), b, 0644)
}

func versionOf(dir string) string {
	return strings.TrimPrefix(filepath.Base(dir), "app-")
}

// compareVersion 粗略比较 "4.23118.1" 这类版本号，返回 -1/0/1
func compareVersion(a, b string) int {
	pa := strings.Split(a, ".")
	pb := strings.Split(b, ".")
	n := len(pa)
	if len(pb) > n {
		n = len(pb)
	}
	for i := 0; i < n; i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x < y {
			return -1
		}
		if x > y {
			return 1
		}
	}
	return 0
}

func warnNewerVersion(dirs []string, st *State) {
	if st == nil || st.Version == "" {
		return
	}
	newer := ""
	for _, d := range dirs {
		v := versionOf(d)
		if compareVersion(v, st.Version) > 0 {
			newer = v
		}
	}
	if newer != "" {
		logf("[注意] 检测到更新版本 app-%s，上次汉化针对的是 app-%s。\n", newer, st.Version)
		logf("       旧版本资源不通用，需要对这个新版本重新汉化。\n\n")
	}
}

func loadCatalog() (map[string]string, error) {
	r, err := gzip.NewReader(bytes.NewReader(zhGz))
	if err != nil {
		return nil, fmt.Errorf("解压词库失败: %w", err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("读取词库失败: %w", err)
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("解析词库失败: %w", err)
	}
	return m, nil
}

func findEntry(e *Entry, prefix, target string) *Entry {
	if e == nil || e.Files == nil {
		return nil
	}
	for name, child := range e.Files {
		p := prefix + name
		if child.Files != nil {
			if r := findEntry(child, p+"/", target); r != nil {
				return r
			}
		} else if p == target {
			return child
		}
	}
	return nil
}

// openAsarRaw 读取 asar 头部结构，返回原始缓冲区、头部文本和根目录
func openAsarRaw(ap string) (*AsarInfo, *Entry, error) {
	buf, err := os.ReadFile(ap)
	if err != nil {
		return nil, nil, fmt.Errorf("读取 asar 失败（Claude 可能还在运行，先退出）: %w", err)
	}
	if len(buf) < 16 {
		return nil, nil, errors.New("asar 文件损坏")
	}
	// asar 头部布局: [0:4]=4, [4:8]=headerSize(pickle 总长), [8:12]=headerSize-4,
	// [12:16]=header JSON 字节长度, 之后是 JSON 本体, 然后是 padding 到 8+headerSize
	headerSize := int64(binary.LittleEndian.Uint32(buf[4:8]))
	jsonLen := int64(binary.LittleEndian.Uint32(buf[12:16]))
	dataStart := 8 + headerSize
	headerEnd := 16 + jsonLen
	if headerEnd > int64(len(buf)) {
		return nil, nil, errors.New("asar 头部长度异常")
	}
	headerRaw := string(buf[16:headerEnd])
	if dataStart > int64(len(buf)) || dataStart < headerEnd {
		return nil, nil, errors.New("asar 数据区起点异常")
	}

	var root Entry
	if err := json.Unmarshal([]byte(headerRaw), &root); err != nil {
		return nil, nil, fmt.Errorf("解析 asar 头部失败: %w", err)
	}
	sum := sha256.Sum256([]byte(headerRaw))
	return &AsarInfo{
		Buf:       buf,
		Path:      ap,
		HeaderRaw: headerRaw,
		HeaderEnd: headerEnd,
		DataStart: dataStart,
		HeaderSHA: hex.EncodeToString(sum[:]),
	}, &root, nil
}

// locate 在已解析的 asar 里定位条目，并算出数据绝对偏移
func locate(info *AsarInfo, root *Entry, target string) (*Entry, int64, []byte, error) {
	ent := findEntry(root, "", target)
	if ent == nil {
		return nil, 0, nil, fmt.Errorf("asar 里找不到 %s", target)
	}
	if ent.Integrity == nil {
		return nil, 0, nil, errors.New("该条目没有完整性信息，格式不支持")
	}
	off, err := strconv.ParseInt(ent.Offset, 10, 64)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("offset 解析失败: %w", err)
	}
	abs := info.DataStart + off
	if abs+ent.Size > int64(len(info.Buf)) {
		return nil, 0, nil, errors.New("条目位置越界")
	}
	return ent, abs, info.Buf[abs : abs+ent.Size], nil
}

// openAsar 打开主语言资源包并定位主语言表
func openAsar(ap string) (*AsarInfo, error) {
	info, root, err := openAsarRaw(ap)
	if err != nil {
		return nil, err
	}
	ent, abs, content, err := locate(info, root, targetEntry)
	if err != nil {
		return nil, fmt.Errorf("%v，可能这个版本结构不同", err)
	}
	info.Entry = ent
	info.Abs = abs
	info.Content = content
	return info, nil
}

// countChinese 统计真正的「中文条目」：含汉字且不含日文假名。
// 只数汉字会把日文误判成中文（日文里大量用汉字，如「設定」「変更」），
// 所以必须靠假名（平假名/片假名）来排除日文。
func countChinese(m map[string]string) int {
	c := 0
	for _, v := range m {
		hasCJK, hasKana := false, false
		for _, r := range v {
			if r >= 0x4E00 && r <= 0x9FFF {
				hasCJK = true
			}
			if (r >= 0x3040 && r <= 0x309F) || (r >= 0x30A0 && r <= 0x30FF) || (r >= 0x31F0 && r <= 0x31FF) {
				hasKana = true
			}
		}
		if hasCJK && !hasKana {
			c++
		}
	}
	return c
}

func analyze(d string, st *State) (*Analysis, error) {
	info, err := openAsar(asarPath(d))
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimRight(info.Content, " \t\r\n\x00")
	var m map[string]string
	if err := json.Unmarshal(trimmed, &m); err != nil {
		return nil, fmt.Errorf("解析语言表失败: %w", err)
	}
	cn := countChinese(m)
	a := &Analysis{
		Dir:     d,
		Version: versionOf(d),
		Total:   len(m),
		Chinese: cn,
		Ratio:   float64(cn) / float64(len(m)+1),
		Info:    info,
		State:   st,
	}
	switch {
	case a.Ratio >= 0.90:
		a.Verdict = "already"
	case a.Ratio >= 0.30:
		// 日文原版里也有约 5% 条目不含假名（纯汉字短词），所以门槛不能设太低
		a.Verdict = "partial"
	case st != nil && st.Version == a.Version && st.HeaderHash != info.HeaderSHA:
		a.Verdict = "stale"
	case st != nil && st.Version != "" && compareVersion(a.Version, st.Version) > 0:
		a.Verdict = "upgrade"
	default:
		a.Verdict = "never"
	}
	return a, nil
}

func printAnalysis(a *Analysis) {
	logf("目标: %s\n", a.Dir)
	logf("  语言表: %d 条，其中中文 %d 条（%.1f%%）\n", a.Total, a.Chinese, a.Ratio*100)
	switch a.Verdict {
	case "already":
		logf("  判断: 已经是中文\n")
	case "partial":
		logf("  判断: 只有部分中文，可能上次汉化被部分覆盖\n")
	case "stale":
		logf("  判断: 汉化已失效 —— 文件被替换过，Claude 可能重新下载了同版本\n")
		if a.State != nil {
			logf("        上次汉化: %s（工具 v%s，命中 %d 条）\n",
				formatTime(a.State.PatchedAt), a.State.ToolVersion, a.State.Hit)
		}
	case "upgrade":
		logf("  判断: 这是升级后的新版本，还没汉化\n")
		if a.State != nil {
			logf("        上次汉化的是 app-%s（%s）\n", a.State.Version, formatTime(a.State.PatchedAt))
		}
	case "never":
		logf("  判断: 从未汉化过（原版日文界面）\n")
	}
}

func printStateInfo(st *State) {
	if st == nil {
		logf("\n没有找到汉化记录文件（还没跑过这个工具，或记录被删）\n")
		return
	}
	logf("\n上次汉化记录:\n  时间: %s\n  工具版本: v%s\n  目录: %s\n  命中: %d 条\n",
		formatTime(st.PatchedAt), st.ToolVersion, st.InstallDir, st.Hit)
}

func formatTime(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// buildMerged 把词库合并进目标语言表，输出填充到原槽位的字节。
// 槽位长度固定，短出来的部分用空格补齐，避免改动 asar 布局。
func buildMerged(raw []byte, zh map[string]string) (filled []byte, total, hit int, err error) {
	var ja map[string]string
	if err := json.Unmarshal(bytes.TrimRight(raw, " \t\r\n\x00"), &ja); err != nil {
		return nil, 0, 0, fmt.Errorf("解析目标语言表失败: %w", err)
	}
	merged := make(map[string]string, len(ja))
	for k, v := range ja {
		if nv, ok := zh[k]; ok && strings.TrimSpace(nv) != "" {
			merged[k] = nv
			hit++
		} else {
			merged[k] = v
		}
	}
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	if err := enc.Encode(merged); err != nil {
		return nil, 0, 0, fmt.Errorf("生成新语言表失败: %w", err)
	}
	out := bytes.TrimRight(body.Bytes(), "\n")
	slot := len(raw)
	if len(out) > slot {
		return nil, 0, 0, fmt.Errorf("新语言表 %d 字节，超出槽位 %d 字节", len(out), slot)
	}
	filled = make([]byte, slot)
	copy(filled, out)
	for i := len(out); i < slot; i++ {
		filled[i] = 0x20
	}
	return filled, len(ja), hit, nil
}

// patchAsar 汉化一个资源包里的全部目标语言表，返回旧/新头部哈希与主表命中数
func patchAsar(dir string, t asarTarget, zh map[string]string, dry, noBackup bool) (oldCands []string, newHash string, hit, total int, err error) {
	ap := filepath.Join(dir, "resources", t.Asar)
	info, root, err := openAsarRaw(ap)
	if err != nil {
		return nil, "", 0, 0, err
	}

	type edit struct {
		ent  *Entry
		abs  int64
		data []byte
	}
	var edits []edit
	for _, e := range t.Entries {
		ent, abs, raw, lerr := locate(info, root, e)
		if lerr != nil {
			logf("  [跳过] %s: %v\n", e, lerr)
			continue
		}
		filled, n, h, berr := buildMerged(raw, zh)
		if berr != nil {
			return nil, "", 0, 0, fmt.Errorf("%s: %v", e, berr)
		}
		logf("  %-52s %6d 条，命中 %6d（%.1f%%）\n", e, n, h, 100*float64(h)/float64(n+1))
		if e == targetEntry || (t.Asar == "ion-dist.asar" && e == "i18n/ja-JP.json") {
			hit, total = h, n
		}
		edits = append(edits, edit{ent, abs, filled})
	}
	if len(edits) == 0 {
		return nil, "", 0, 0, errors.New("没有可处理的条目")
	}
	if dry {
		logf("  [dry-run] %s 校验通过，未写入\n", t.Asar)
		return nil, "", hit, total, nil
	}

	if !noBackup {
		if _, serr := os.Stat(ap + ".hanhua-bak"); serr != nil {
			logf("  备份 %s ...\n", t.Asar)
			if cerr := copyFile(ap, ap+".hanhua-bak"); cerr != nil {
				return nil, "", 0, 0, fmt.Errorf("备份 %s 失败: %w", t.Asar, cerr)
			}
		}
	}

	buf := info.Buf
	newHeader := info.HeaderRaw
	for _, ed := range edits {
		copy(buf[ed.abs:ed.abs+ed.ent.Size], ed.data)

		old := ed.ent.Integrity.Hash
		sum := sha256.Sum256(ed.data)
		fresh := hex.EncodeToString(sum[:])

		// 先处理分块哈希：小于一个分块的条目，它的 blocks[0] 和整文件哈希是同一个值，
		// 两条命中的字符串必须一起换掉，所以用 ReplaceAll 而不是限定一次。
		bs := ed.ent.Integrity.BlockSize
		if bs <= 0 {
			bs = 4194304
		}
		var newBlocks []string
		for i := int64(0); i < int64(len(ed.data)); i += bs {
			end := i + bs
			if end > int64(len(ed.data)) {
				end = int64(len(ed.data))
			}
			s := sha256.Sum256(ed.data[i:end])
			newBlocks = append(newBlocks, hex.EncodeToString(s[:]))
		}
		for i, ob := range ed.ent.Integrity.Blocks {
			if i >= len(newBlocks) {
				break
			}
			newHeader = strings.ReplaceAll(newHeader, ob, newBlocks[i])
		}

		if strings.Count(newHeader, old) > 0 {
			newHeader = strings.ReplaceAll(newHeader, old, fresh)
		} else if !strings.Contains(newHeader, fresh) {
			// 分块替换没顺带覆盖整文件哈希，说明结构异常
			return nil, "", 0, 0, fmt.Errorf("%s 条目哈希在头部找不到，放弃", t.Asar)
		}
	}

	if len(newHeader) != len(info.HeaderRaw) {
		return nil, "", 0, 0, errors.New("头部长度变化，放弃")
	}
	copy(buf[16:info.HeaderEnd], []byte(newHeader))
	if werr := os.WriteFile(ap, buf, 0644); werr != nil {
		return nil, "", 0, 0, fmt.Errorf("写入 %s 失败: %w", t.Asar, werr)
	}
	fs := sha256.Sum256([]byte(newHeader))
	nh := hex.EncodeToString(fs[:])
	logf("  头部哈希 %s -> %s\n", info.HeaderSHA[:12], nh[:12])

	// exe 里记录的旧头部哈希可能是任意历史值：原版、上一次汉化的结果，或者
	// 上次写 exe 失败时留下的中间态。所以把「当前值 + 备份原版值」都当候选，
	// 由 patchExe 在 exe 里逐个试，命中哪个就换哪个。
	cands := []string{info.HeaderSHA}
	if o, herr := asarHeaderHash(ap + ".hanhua-bak"); herr == nil {
		cands = append(cands, o)
	}
	return cands, nh, hit, total, nil
}

// asarHeaderHash 只读 asar 头部并算哈希，用于取原版备份的头部哈希
func asarHeaderHash(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	head := make([]byte, 16)
	if _, err := io.ReadFull(f, head); err != nil {
		return "", err
	}
	jsonLen := int64(binary.LittleEndian.Uint32(head[12:16]))
	if jsonLen <= 0 || jsonLen > 64<<20 {
		return "", errors.New("头部长度异常")
	}
	body := make([]byte, jsonLen)
	if _, err := io.ReadFull(f, body); err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

// hashRep 一次 exe 哈希替换：Olds 是候选旧值，命中哪个就用哪个。
type hashRep struct {
	Olds []string
	New  string
}

// patchExe 把 exe 里记录的 asar 头部哈希换成新值。所有替换一次性读写。
func patchExe(ep string, reps []hashRep, noBackup bool) error {
	if len(reps) == 0 {
		return nil
	}
	if !noBackup {
		if _, err := os.Stat(ep + ".hanhua-bak"); err != nil {
			logf("  备份 claude.exe ...\n")
			if err := copyFile(ep, ep+".hanhua-bak"); err != nil {
				return fmt.Errorf("备份 exe 失败: %w", err)
			}
		}
	}
	exeBuf, err := os.ReadFile(ep)
	if err != nil {
		return fmt.Errorf("读取 exe 失败: %w", err)
	}
	for _, r := range reps {
		if len(r.New) != 64 {
			return errors.New("哈希长度异常")
		}
		applied := false
		for _, oldH := range r.Olds {
			if len(oldH) != 64 {
				continue
			}
			if bytes.Count(exeBuf, []byte(oldH)) != 1 {
				continue
			}
			idx := bytes.Index(exeBuf, []byte(oldH))
			copy(exeBuf[idx:idx+64], []byte(r.New))
			applied = true
			break
		}
		if !applied {
			if bytes.Contains(exeBuf, []byte(r.New)) {
				logf("  exe 里已经是新哈希 %s，跳过\n", r.New[:12])
				continue
			}
			return fmt.Errorf("exe 里找不到对应的旧头部哈希（版本结构不同）")
		}
	}
	if err := os.WriteFile(ep, exeBuf, 0644); err != nil {
		return fmt.Errorf("写入 exe 失败: %w", err)
	}
	logf("  exe 内 %d 处头部哈希已同步\n", len(reps))
	return nil
}

func doRestore(d string) error {
	n := 0
	for _, t := range patchTargets {
		ap := filepath.Join(d, "resources", t.Asar)
		bak := ap + ".hanhua-bak"
		if _, err := os.Stat(bak); err != nil {
			continue
		}
		if err := os.Rename(bak, ap); err != nil {
			if err := copyFile(bak, ap); err != nil {
				return err
			}
			os.Remove(bak)
		}
		n++
	}
	ep := exePath(d)
	if _, err := os.Stat(ep + ".hanhua-bak"); err == nil {
		if err := os.Rename(ep+".hanhua-bak", ep); err != nil {
			if err := copyFile(ep+".hanhua-bak", ep); err != nil {
				return err
			}
			os.Remove(ep + ".hanhua-bak")
		}
		n++
	}
	if n == 0 {
		return errors.New("没有找到备份，无法还原")
	}
	os.Remove(statePath())
	logf("[完成] 已还原 %d 个文件: %s\n", n, d)
	return nil
}

func doPatch(a *Analysis, dry, noBackup bool) error {
	d := a.Dir
	zh, err := loadCatalog()
	if err != nil {
		return err
	}
	logf("  内置词库: %d 条\n", len(zh))

	var reps []hashRep
	var mainNewHash string
	mainHit, mainTotal := 0, 0
	st0 := loadState()
	for _, t := range patchTargets {
		cands, nh, h, n, perr := patchAsar(d, t, zh, dry, noBackup)
		if perr != nil {
			logf("  [失败] %s: %v\n", t.Asar, perr)
			continue
		}
		if t.Asar == "ion-dist.asar" {
			mainNewHash = nh
			mainHit, mainTotal = h, n
		}
		if !dry && nh != "" {
			// 上次打补丁写进 exe 的哈希也算候选，重复运行才对得上
			if st0 != nil && st0.HeaderHash != "" {
				cands = append(cands, st0.HeaderHash)
			}
			reps = append(reps, hashRep{Olds: cands, New: nh})
		}
	}
	if dry {
		return nil
	}
	if len(reps) == 0 {
		return errors.New("没有任何资源包被成功处理")
	}
	if err := patchExe(exePath(d), reps, noBackup); err != nil {
		return err
	}

	st, _ := os.Stat(asarPath(d))
	var mtime int64
	if st != nil {
		mtime = st.ModTime().Unix()
	}
	_ = saveState(&State{
		ToolVersion: toolVer,
		PatchedAt:   time.Now().Format(time.RFC3339),
		InstallDir:  d,
		Version:     versionOf(d),
		HeaderHash:  mainNewHash,
		AsarSize:    int64(len(a.Info.Buf)),
		AsarMTime:   mtime,
		Entries:     mainTotal,
		Hit:         mainHit,
	})

	logf("  [完成] 汉化成功，重新打开 Claude 即可\n")
	logf("  备份: resources\\*.hanhua-bak 与 claude.exe.hanhua-bak\n")
	return nil
}
