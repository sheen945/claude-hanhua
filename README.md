# Claude 桌面版一键汉化（Windows）

把 Windows 版 Claude Desktop 的日文/英文界面换成简体中文，一个 exe，双击就行。

## 这是什么

官方 Claude Desktop 的语言包只有 11 种，**没有中文**。你如果在日本区或者账号 locale 被设成 `ja-JP`，整个界面就是日文。

这个工具把内置的 44,199 条中文词库注入到 Claude 的语言资源包（`ion-dist.asar`）里，并同步修正 Electron 的三层完整性校验，让应用正常启动、界面显示中文。

工具启动时会先自检，再决定要不要动手：

- **已汉化且文件没变** → 提示「已是中文」，直接跳过
- **Claude 升级过、资源被换掉** → 提示「检测到新版本」，自动重新汉化
- **有多个版本目录** → 逐个判断，只处理需要处理的

**当前效果：44,199 条词库，Claude 5.1231.2 实测 100% 汉化（43,714 条全部命中）**

| 原日文 | 汉化后 |
|---|---|
| 新規タスク - Claude | 新会话 - Claude |
| サイドバーを非表示 | 隐藏侧边栏 |
| 新規作成 / 検索 / カスタマイズ | 新建 / 搜索 / 自定义 |
| ようこそ！ | 欢迎！ |
| Claude へのプロンプトを入力してください | 请输入给 Claude 的提示词 |
| ファイルやコネクタなどを追加 | 添加文件、连接器等 |
| タスクを開始 | 开始任务 |
| プロバイダーに認証情報が拒否されました | 提供方拒绝了凭据。请在「设置」中重新输入。 |

## 用法

**最简单**：关掉 Claude，双击 `claude-hanhua.exe`，等它跑完，再打开 Claude。

```
claude-hanhua.exe              自动检测：已是中文就跳过，升级过就重打
claude-hanhua.exe -status      只看状态诊断（是否汉化过、是否升级了）
claude-hanhua.exe -list        列出检测到的 Claude 安装
claude-hanhua.exe -dry         只检查不写入（看命中率）
claude-hanhua.exe -dir <路径>  指定安装目录
claude-hanhua.exe -restore     还原成原版
claude-hanhua.exe -force       强制重新汉化（忽略「已是中文」判断）
claude-hanhua.exe -no-backup   不备份（不建议）
```

**升级自检是怎么做的**：每次汉化成功会写一份记录到

```
%LOCALAPPDATA%\AnthropicClaude\claude-hanhua.state.json
```

里面存着当时的版本号、资源包头部哈希、命中条数。下次运行就是拿这份记录和磁盘现状对比——哈希对不上说明文件被换过，版本号变大了说明换了新版本。同时也会统计当前语言表里「含汉字且不含日文假名」的条目占比，判断到底有没有中文。两路信息交叉验证，避免误判。

安装目录默认在：

```
%LOCALAPPDATA%\AnthropicClaude\app-<版本>\
```

## 它到底改了什么

Claude 的资源包是 Electron 的 `asar` 格式，改里面的文件会踩到三层校验，少改一层应用直接闪退：

1. **条目级 SHA256**：asar 头部 JSON 里每个文件有自己的 `integrity.hash`
2. **分块哈希**：大于 4MB 的条目还有 `blocks` 数组，每 4MB 一个 SHA256
3. **头部哈希**：整个头部 JSON 的 SHA256 被嵌在 `claude.exe` 的 PE 资源里

工具的处理顺序：

1. 读 `resources\ion-dist.asar`，定位 `i18n/ja-JP.json` 条目（4.48MB）
2. 用内置中文词库按 key 合并：命中用中文，没命中保留原文
3. 写回原槽位，不足部分空格填充（保持长度不变）
4. 重算条目 SHA256 + 每个 4MB 分块的 SHA256，替换头部 JSON 里的旧值
5. 重算头部 SHA256，替换 `claude.exe` 里记录的旧值
6. 备份原文件为 `*.hanhua-bak`，出问题可以 `-restore`

## 备份与还原

首次汉化会自动备份：

```
resources\ion-dist.asar.hanhua-bak   原版资源包（203 MB）
claude.exe.hanhua-bak                原版 exe（247 MB）
```

还原：

```
claude-hanhua.exe -restore
```

## 注意事项

**Claude 一升级，汉化就失效——但工具能自己发现。** 官方更新会把 `ion-dist.asar` 和 `claude.exe` 整个换掉，并装到一个新的 `app-<版本>` 目录里。下次运行工具时会自动识别「新版本未汉化」并处理，旧版本目录里的汉化不受影响。如果新版本改了 key，命中率会下降但不会崩——没命中的条目保留原文，跑 `-dry` 就能看到命中多少。

**只对 Squirrel 安装版有效。** 也就是 `%LOCALAPPDATA%\AnthropicClaude\app-*` 这种目录结构。Microsoft Store 的 MSIX 版装在 `C:\Program Files\WindowsApps`，受系统保护，工具默认不改它。

**界面语言取决于账号 locale。** 工具改的是 `ja-JP` 语言表。如果你的 Claude 界面不是日文（比如英文），那它读的是 `en-US.json`，这个工具就不适用。判断方法：跑 `-dry`，如果显示「目标语言表: N 条」且命中率很高，就说明对上了。

**汉化不影响功能，但也不改变区域限制。** 如果你的账号碰到「Claude isn't available in your region」，那是账号/网络层面的问题，跟界面语言无关。

## 词库来源

- 约 6,959 条来自社区项目 [ooac/Claude-desk-cn](https://github.com/ooac/Claude-desk-cn)（macOS 版汉化补丁）
- 其余约 37,240 条为针对 Windows 版 key 体系的批量翻译（含 Claude 5.1231.2 新增的 1,572 条），严格保留 ICU MessageFormat 占位符（`{count, plural, ...}`）和 HTML 标签结构

翻译校验规则（每批都跑）：

- 键集合与原文完全一致
- 每个值里的 `{变量名}` 集合与原文一致
- ICU 的 plural / select / number 结构不动
- HTML 标签（`<b>`、`<link>`、`<code>`）不丢
- 产品名保持英文（Claude、Anthropic、Artifacts、Cowork、Slack、AWS、MCP…）

## 自己编译

需要 Go 1.21+：

```powershell
$env:CGO_ENABLED='0'
go build -ldflags="-s -w" -trimpath -o claude-hanhua.exe .
```

词库 `zh.json.gz` 通过 `go:embed` 编进 exe，所以产物是单文件、无运行时依赖的。

## 免责声明

这是非官方的界面语言修改工具，修改的是本地安装文件，不涉及账号、不联网、不上传任何数据。使用前建议先备份，风险自负。Claude 是 Anthropic 的产品，本项目与 Anthropic 无关。

## License

MIT
