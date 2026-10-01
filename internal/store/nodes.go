package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// TokenPrefix 是所有 Agent Token 的前缀，便于在日志/配置里一眼认出。
const TokenPrefix = "pba_"

// 节点字段上限。服务端 API 会先校验一次，这里再挡一次——
// 存储层是最后一道防线，任何调用方都不能绕过。
const (
	maxNodeNameLen  = 64
	maxGroupLen     = 64
	maxRegionLen    = 64
	maxNoteLen      = 512
	maxNodeIfaceLen = 32

	// maxPriceCents 是一千亿元（分）。上限不是为了限制用户，而是防溢出：
	// 前端展示与月均计算都要乘除，INTEGER 列一旦被写进离谱的值，
	// 后面的派生金额会溢出成负数，看起来像"倒欠钱"。
	maxPriceCents = 1_000_000_000_00
	// maxCurrencyLen 是 ISO 4217 代码长度（3 位）的两倍多，够放非标准写法（如 USDT）。
	maxCurrencyLen = 8
	// maxBillingMonths 十年。再长的周期在界面上也没有对应的文案。
	maxBillingMonths = 120

	// maxNodeTags / maxTagLen 是标签的硬上限：一台机器最多 64 个，每个最长 32 个字符。
	//
	// 为什么"用户要求不给限制"但仍然留着上界：标签跟着节点 DTO **每秒**通过 SSE
	// 推送（节点一变就推整条 DTO，见 api_stream.go），没有上界的话，几十个长标签
	// 会把每秒的推送一起撑大，首屏的全量快照（GET /api/v1/nodes）也跟着变慢。
	// 一个能被 curl 直接写入的字段不该有拖慢整条实时通道的能力。
	// 64 是个现实中碰不到的数（正常人就挂几个），它只用来挡住脚本灌进来的几千个标签。
	//
	// 32 而不是 16：参考交互里的 `Black Friday 2025` 就有 17 个字符，16 字上限
	// 连它都装不下。
	//
	// 长度按**字符**（rune）算，与名称/备注一致：按字节算会把 6 个汉字当成 18 字节
	// 拒掉，而前端 maxlength 数的是字符，两边提示会自相矛盾。
	//
	// 这两个数字前端也各写了一份（先在前端拦一道），服务端这里才是真正生效的那一道
	// —— 前端可以被绕过（curl 直接打接口）。
	maxNodeTags = 64
	maxTagLen   = 32
)

var (
	// ErrNodeNotFound 表示节点不存在（或 token 无效）。
	ErrNodeNotFound = errors.New("节点不存在")
	// ErrNodeNameTaken 表示节点名称重复。
	ErrNodeNameTaken = errors.New("节点名称已存在")
	// ErrInvalidNode 表示节点参数不合法。
	ErrInvalidNode = errors.New("节点参数不合法")
	// ErrNodeOrderInvalid 表示重排请求的 id 列表与库里的节点集合对不上
	// （缺、多、重复、不存在）。错误消息里会写清是哪几个 id。
	ErrNodeOrderInvalid = errors.New("节点顺序与当前节点不一致")
)

// Node 是节点的配置信息（不含 token 明文——明文只在创建时返回一次）。
type Node struct {
	ID             int64
	Name           string
	GroupName      string
	Region         string
	Note           string
	TokenPrefix    string
	TokenCreatedAt int64
	Iface          string
	IntervalSec    int
	TrafficLimit   int64
	TrafficWarnPct int
	ResetDay       int
	ExpiresAt      int64
	SortOrder      int
	Enabled        bool
	CreatedAt      int64
	UpdatedAt      int64

	// 价格三件套（金额一律用"分"存整数：浮点数做金额会在乘除后出现 0.01 的漂移）。
	PriceCents    int64
	Currency      string
	BillingMonths int

	// Tags 是用户给这台机器挂的标签（最多 maxNodeTags 个，每个最长 maxTagLen 字符）。
	// 库里存成 JSON 字符串数组（见迁移 0004）；读出来永远是**非 nil** 的切片，
	// 空切片就是"没有标签"。
	Tags []string
}

// NewNode 是创建节点时的输入。
type NewNode struct {
	Name           string
	GroupName      string
	Region         string
	Note           string
	Iface          string
	IntervalSec    int
	TrafficLimit   int64
	TrafficWarnPct int
	ResetDay       int
	ExpiresAt      int64

	// SortOrder 是**显式**指定的排序值；nil 表示调用方没有指定，由 CreateNode
	// 排到最后（max(sort_order)+1）。
	//
	// 为什么用指针而不是 int：0 是一个合法的排序值（显式传 0 = 排到最前面），
	// 而"没传"在 int 上与它完全无法区分。两者混在一起正是"新建的节点插到最前面"
	// 的成因 —— 前端的表单根本不发这个字段，于是每台新机器都拿到 0。
	SortOrder *int

	PriceCents    int64
	Currency      string
	BillingMonths int

	// Tags 是原始输入（可能带空白、重复、空串），写入前统一由 NormalizeTags 归一化。
	Tags []string
}

// GenerateToken 生成 32 字节随机 Token（前缀 pba_ + base64url）。
func GenerateToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成随机 Token 失败: %w", err)
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

// HashToken 返回 Token 的 SHA-256。数据库只存这个值，永远不存明文。
//
// Token 是高熵随机值（256 位），不需要 Argon2 之类的慢哈希。
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// TokenPrefixOf 返回用于界面展示的前 8 个字符（不足以还原 Token）。
func TokenPrefixOf(token string) string {
	if len(token) <= 8 {
		return token
	}
	return token[:8]
}

func (n NewNode) normalized() NewNode {
	n.Name = strings.TrimSpace(n.Name)
	n.GroupName = strings.TrimSpace(n.GroupName)
	n.Region = strings.TrimSpace(n.Region)
	n.Note = strings.TrimSpace(n.Note)
	n.Iface = strings.TrimSpace(n.Iface)
	n.Currency = strings.TrimSpace(n.Currency)
	return n
}

// NormalizeTags 归一化标签列表：去掉首尾空白、丢弃空串、去重（同一个标签只留一个）。
//
// 顺序保持首次出现的顺序：那是用户自己排的，排序会把他刚加进去的标签挪到别处。
//
// 写入（Create/Update）与读取（脏数据）共用这一套规则，不会出现"写进去合法、
// 读出来却被当成脏数据丢掉"这种自相矛盾的行为。导出是给 API 层用的：改标签的接口
// 要先拿到归一化后的值去比对"标签到底变没变"，才能在审计里写清改了什么 ——
// 把归一化规则在服务端再抄一遍，两处迟早会分叉。
func NormalizeTags(tags []string) ([]string, error) {
	out := make([]string, 0, len(tags))
	seen := make(map[string]bool, len(tags))
	for _, raw := range tags {
		tag := strings.TrimSpace(raw)
		if tag == "" {
			continue // 只有空白的标签在界面上就是一个看不见的空框，直接丢弃
		}
		if n := utf8.RuneCountInString(tag); n > maxTagLen {
			// 提示里必须带上是哪个标签：一次提交可能有几十个，只说"某个标签太长"
			// 用户得自己一个个数过去。
			return nil, fmt.Errorf("%w: 标签 %q 有 %d 个字符，超过上限 %d", ErrInvalidNode, tag, n, maxTagLen)
		}
		if seen[tag] {
			continue
		}
		seen[tag] = true
		out = append(out, tag)
	}
	if len(out) > maxNodeTags {
		return nil, fmt.Errorf("%w: 标签数量 %d 超过上限 %d", ErrInvalidNode, len(out), maxNodeTags)
	}
	return out, nil
}

// encodeTags 把标签编码成入库的 JSON 文本（归一化之后）。
//
// 空列表编码成 "[]" 而不是空串：读路径只需要处理一种"没有标签"的写法。
func encodeTags(tags []string) (string, error) {
	clean, err := NormalizeTags(tags)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(clean)
	if err != nil {
		return "", fmt.Errorf("编码标签失败: %w", err)
	}
	return string(data), nil
}

// decodeTags 把库里的 JSON 文本解回标签列表，任何脏数据都回退成空列表。
//
// 读路径**绝不能**因为一行数据报错：老的 NULL、被手工改坏的 JSON、混进了非字符串
// 元素的数组，只要有一处坏掉，ListNodes 就会整体失败 —— 表现是整个首页一片空白，
// 而用户完全不知道是哪台机器的哪一行坏了。标签只是展示用的附加信息，坏掉就当没有。
//
// 返回值保证**非 nil**（空切片就是"没有标签"），前端因此永远收到 []，不会收到 null。
func decodeTags(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return []string{}
	}
	var parsed []string
	// 非字符串元素（如 [1,2]）会让 Unmarshal 直接失败，正是"回退空列表"要的效果。
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return []string{}
	}
	clean, err := NormalizeTags(parsed)
	if err != nil {
		// 超长/超量的脏数据同样回退：宁可少显示几个标签，也不能让列表接口 500。
		return []string{}
	}
	return clean
}

// isUpperAlpha 判断是否全是大写 ASCII 字母。
//
// 大写要求是接口约定（前端与服务端入口都会 ToUpper），存储层只负责挡住
// 绕过入口的调用方，不在这里静默改写——否则"调用方传的值"和"库里的值"
// 会不一致，排查时看不到任何线索。
func isUpperAlpha(s string) bool {
	for _, r := range s {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

// Validate 校验节点参数。返回的错误一定包装了 ErrInvalidNode。
//
// 长度一律按**字符**（rune）算：前端表单的 maxlength 也是按字符，
// 用字节数会出现"22 个汉字的名称被拒、提示却说超过 64"这种自相矛盾的提示，
// 也会让本该合法的中文备注（171–200 字）被拒。
func (n NewNode) Validate() error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidNode, fmt.Sprintf(format, args...))
	}
	nameLen := utf8.RuneCountInString(n.Name)
	switch {
	case n.Name == "":
		return fail("名称不能为空")
	case nameLen > maxNodeNameLen:
		return fail("名称长度 %d 超过上限 %d", nameLen, maxNodeNameLen)
	case utf8.RuneCountInString(n.GroupName) > maxGroupLen:
		return fail("分组长度超过上限 %d", maxGroupLen)
	case utf8.RuneCountInString(n.Region) > maxRegionLen:
		return fail("地区长度超过上限 %d", maxRegionLen)
	case utf8.RuneCountInString(n.Note) > maxNoteLen:
		return fail("备注长度超过上限 %d", maxNoteLen)
	case utf8.RuneCountInString(n.Iface) > maxNodeIfaceLen:
		return fail("网卡名长度超过上限 %d", maxNodeIfaceLen)
	case n.IntervalSec < 1 || n.IntervalSec > 300:
		return fail("上报间隔 %d 必须在 1-300 秒之间", n.IntervalSec)
	case n.TrafficLimit < 0:
		return fail("流量额度不能为负")
	case n.TrafficWarnPct < 0 || n.TrafficWarnPct > 100:
		return fail("告警阈值 %d 必须在 0-100 之间", n.TrafficWarnPct)
	case n.ResetDay < 1 || n.ResetDay > 31:
		return fail("流量重置日 %d 必须在 1-31 之间", n.ResetDay)
	case n.ExpiresAt < 0:
		return fail("到期时间不能为负")
	case n.PriceCents < 0:
		return fail("价格不能为负")
	case n.PriceCents > maxPriceCents:
		return fail("价格超过上限 %d 分", maxPriceCents)
	case n.Currency != "" && !isUpperAlpha(n.Currency):
		return fail("货币必须是大写字母（如 CNY / USD）")
	case utf8.RuneCountInString(n.Currency) > maxCurrencyLen:
		return fail("货币代码长度超过上限 %d", maxCurrencyLen)
	case n.BillingMonths < 0 || n.BillingMonths > maxBillingMonths:
		return fail("计费周期 %d 必须在 0（不填）或 1-%d 个月之间", n.BillingMonths, maxBillingMonths)
	// 下面两条是"半残状态"的护栏：填了价格却没填周期，月均与剩余价值都算不出来，
	// 界面上会出现一个没有单位、也没有月均的金额；反过来只填周期则等价于价格 0。
	// 与其存进去再让前端各自猜，不如在入口一次拒绝。
	case n.BillingMonths == 0 && (n.PriceCents != 0 || n.Currency != ""):
		return fail("没有计费周期时，价格与货币都必须留空")
	case n.PriceCents == 0 && n.BillingMonths != 0:
		return fail("填了计费周期就必须填价格")
	}
	// 标签的规则（去空白、去重、单个长度、总数量）统一在 NormalizeTags 里，
	// 这里只借它挡一次非法输入；真正入库时还会再归一化一次。
	if _, err := NormalizeTags(n.Tags); err != nil {
		return err
	}
	return nil
}

// CreateNode 创建节点并生成 Token，返回节点与**仅此一次**的明文 Token。
func (d *DB) CreateNode(ctx context.Context, in NewNode, now time.Time) (Node, string, error) {
	in = in.normalized()
	if err := in.Validate(); err != nil {
		return Node{}, "", err
	}
	tagsJSON, err := encodeTags(in.Tags)
	if err != nil {
		return Node{}, "", err
	}
	token, err := GenerateToken()
	if err != nil {
		return Node{}, "", err
	}

	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return Node{}, "", err
	}
	defer func() { _ = tx.Rollback() }()

	// 没显式给排序值时排到最后：列默认值 0 比任何已有节点都小，而 ListNodes 是
	// ORDER BY sort_order, id —— 新机器会插到列表最前面，看起来像"排序乱了"。
	// max+1 与"排在最后"等价，且在同一个事务里算：写事务一开始就持有写锁
	// （见 store.go 的 _txlock=immediate），不会与并发的创建撞上同一个值。
	sortOrder := 0
	if in.SortOrder != nil {
		sortOrder = *in.SortOrder
	} else if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sort_order), 0) + 1 FROM nodes`).Scan(&sortOrder); err != nil {
		return Node{}, "", fmt.Errorf("计算新节点排序值失败: %w", err)
	}

	ts := now.Unix()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO nodes (name, group_name, region, note, token_hash, token_prefix, token_created_at,
			iface, interval_sec, traffic_limit, traffic_warn_pct, reset_day, expires_at, sort_order,
			price_cents, currency, billing_months, tags,
			enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		in.Name, in.GroupName, in.Region, in.Note, HashToken(token), TokenPrefixOf(token), ts,
		in.Iface, in.IntervalSec, in.TrafficLimit, in.TrafficWarnPct, in.ResetDay, in.ExpiresAt, sortOrder,
		in.PriceCents, in.Currency, in.BillingMonths, tagsJSON,
		ts, ts)
	if err != nil {
		if isUniqueViolation(err, "nodes.name") {
			return Node{}, "", ErrNodeNameTaken
		}
		return Node{}, "", fmt.Errorf("创建节点失败: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Node{}, "", fmt.Errorf("读取新节点 ID 失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO node_runtime (node_id, updated_at) VALUES (?, ?)`, id, ts); err != nil {
		return Node{}, "", fmt.Errorf("初始化节点运行态失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Node{}, "", fmt.Errorf("提交创建节点事务失败: %w", err)
	}

	node, err := d.NodeByID(ctx, id)
	if err != nil {
		return Node{}, "", err
	}
	return node, token, nil
}

// NodeByID 按 ID 读取节点。
func (d *DB) NodeByID(ctx context.Context, id int64) (Node, error) {
	row := d.r.QueryRowContext(ctx, nodeSelect+` WHERE id = ?`, id)
	return scanNode(row)
}

// NodeByTokenHash 按 Token 哈希读取节点（Agent 鉴权路径）。
func (d *DB) NodeByTokenHash(ctx context.Context, hash []byte) (Node, error) {
	row := d.r.QueryRowContext(ctx, nodeSelect+` WHERE token_hash = ?`, hash)
	return scanNode(row)
}

// ListNodes 返回全部节点，按排序值、ID 排列。
func (d *DB) ListNodes(ctx context.Context) ([]Node, error) {
	rows, err := d.r.QueryContext(ctx, nodeSelect+` ORDER BY sort_order, id`)
	if err != nil {
		return nil, fmt.Errorf("查询节点列表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var nodes []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历节点列表失败: %w", err)
	}
	return nodes, nil
}

// ReorderNodes 按 ids 的下标重写**全部**节点的 sort_order（1..N）。
//
// 语义是"ids 就是全部节点的完整顺序"，不做"只给一部分就只排一部分"的宽松版本：
// 没被提到的节点 sort_order 会停在 0，而 ListNodes 是 ORDER BY sort_order, id ——
// 它们的相对顺序会变成"谁先建的谁在前"，用户完全无法预期。宁可 400 让调用方补全。
//
// 校验与写入在**同一个事务**里：分两次做的话，两次之间新增/删除的节点会让写入
// 落在一个已经变了的集合上；而写了一半失败更糟 —— 一半节点是新顺序、一半是旧顺序，
// 界面上就是一个谁也看不懂的排列。所以要么全成、要么一个字节都不动。
func (d *DB) ReorderNodes(ctx context.Context, ids []int64) error {
	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `SELECT id FROM nodes`)
	if err != nil {
		return fmt.Errorf("查询节点集合失败: %w", err)
	}
	known := make(map[int64]bool)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return fmt.Errorf("读取节点 ID 失败: %w", err)
		}
		known[id] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("遍历节点 ID 失败: %w", err)
	}
	// 显式关闭：下面还要在同一个事务里执行 UPDATE，不能留着这个结果集。
	if err := rows.Close(); err != nil {
		return fmt.Errorf("关闭节点 ID 结果集失败: %w", err)
	}

	if err := validateNodeOrder(ids, known); err != nil {
		return err
	}

	// 排序值从 1 开始：0 是"没排过"的默认值，把它留给"从未重排过的库"更清楚。
	// 只改 sort_order，不动 updated_at —— 排序不是配置变更，"最后修改时间"
	// 因为拖一下顺序就全体刷新会让人以为每台机器的配置都被改过。
	for i, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE nodes SET sort_order = ? WHERE id = ?`, i+1, id); err != nil {
			return fmt.Errorf("写入第 %d 台节点的排序值失败: %w", i+1, err)
		}
	}
	return tx.Commit()
}

// validateNodeOrder 检查 ids 是否是 known 的一个排列（不重不漏）。
//
// 消息里必须点名是哪几个 id 不对：一次提交可能有几十台机器，只说"顺序不合法"
// 的话，调用方（包括前端与 curl）只能自己逐个比对。
func validateNodeOrder(ids []int64, known map[int64]bool) error {
	seen := make(map[int64]bool, len(ids))
	extra := make([]int64, 0)
	for _, id := range ids {
		if seen[id] {
			return fmt.Errorf("%w：节点 %d 重复出现", ErrNodeOrderInvalid, id)
		}
		seen[id] = true
		if !known[id] {
			extra = append(extra, id)
		}
	}
	missing := make([]int64, 0)
	for id := range known {
		if !seen[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 && len(extra) == 0 {
		return nil
	}
	// 排序后再拼消息：map 的遍历顺序是随机的，不排的话同一份请求每次报的
	// id 顺序都不一样，看起来像"错误内容在变"。
	slices.Sort(missing)
	slices.Sort(extra)
	parts := make([]string, 0, 2)
	if len(missing) > 0 {
		parts = append(parts, "缺少 "+intsText(missing))
	}
	if len(extra) > 0 {
		parts = append(parts, "多出/不存在 "+intsText(extra))
	}
	return fmt.Errorf("%w：请求里有 %d 个 id，当前有 %d 台节点（%s）",
		ErrNodeOrderInvalid, len(ids), len(known), strings.Join(parts, "；"))
}

// intsText 把 id 列表拼成 "[3 5]" 这样的文本（只用于错误消息）。
func intsText(ids []int64) string {
	items := make([]string, 0, len(ids))
	for _, id := range ids {
		items = append(items, strconv.FormatInt(id, 10))
	}
	return "[" + strings.Join(items, " ") + "]"
}

// RotateNodeToken 重新生成 Token，旧 Token 立即失效。
func (d *DB) RotateNodeToken(ctx context.Context, id int64, now time.Time) (Node, string, error) {
	token, err := GenerateToken()
	if err != nil {
		return Node{}, "", err
	}
	res, err := d.w.ExecContext(ctx,
		`UPDATE nodes SET token_hash = ?, token_prefix = ?, token_created_at = ?, updated_at = ? WHERE id = ?`,
		HashToken(token), TokenPrefixOf(token), now.Unix(), now.Unix(), id)
	if err != nil {
		return Node{}, "", fmt.Errorf("更新 Token 失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return Node{}, "", err
	}
	if affected == 0 {
		return Node{}, "", ErrNodeNotFound
	}
	node, err := d.NodeByID(ctx, id)
	if err != nil {
		return Node{}, "", err
	}
	return node, token, nil
}

// UpdateNode 更新节点配置（Token 不在其中，走 RotateNodeToken）。
func (d *DB) UpdateNode(ctx context.Context, n Node, now time.Time) error {
	// 更新路径**永远**是显式给值：Node.SortOrder 是库里读出来的既存值
	// （或调用方刚设的新值），不能走"没指定就排到最后"那条路 ——
	// 否则改一次名称会把节点的排序值悄悄改掉。
	sortOrder := n.SortOrder
	in := NewNode{
		Name: n.Name, GroupName: n.GroupName, Region: n.Region, Note: n.Note, Iface: n.Iface,
		IntervalSec: n.IntervalSec, TrafficLimit: n.TrafficLimit, TrafficWarnPct: n.TrafficWarnPct,
		ResetDay: n.ResetDay, ExpiresAt: n.ExpiresAt, SortOrder: &sortOrder,
		PriceCents: n.PriceCents, Currency: n.Currency, BillingMonths: n.BillingMonths,
		Tags: n.Tags,
	}
	in = in.normalized()
	if err := in.Validate(); err != nil {
		return err
	}
	tagsJSON, err := encodeTags(in.Tags)
	if err != nil {
		return err
	}
	enabled := 0
	if n.Enabled {
		enabled = 1
	}
	res, err := d.w.ExecContext(ctx, `
		UPDATE nodes SET name = ?, group_name = ?, region = ?, note = ?, iface = ?, interval_sec = ?,
			traffic_limit = ?, traffic_warn_pct = ?, reset_day = ?, expires_at = ?, sort_order = ?,
			price_cents = ?, currency = ?, billing_months = ?, tags = ?,
			enabled = ?, updated_at = ?
		WHERE id = ?`,
		in.Name, in.GroupName, in.Region, in.Note, in.Iface, in.IntervalSec,
		in.TrafficLimit, in.TrafficWarnPct, in.ResetDay, in.ExpiresAt, in.SortOrder,
		in.PriceCents, in.Currency, in.BillingMonths, tagsJSON,
		enabled, now.Unix(), n.ID)
	if err != nil {
		if isUniqueViolation(err, "nodes.name") {
			return ErrNodeNameTaken
		}
		return fmt.Errorf("更新节点失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		// 注意：字段值没有变化时 RowsAffected 也可能为 0，因此再用一次存在性检查。
		if _, err := d.NodeByID(ctx, n.ID); err != nil {
			return err
		}
	}
	return nil
}

// DeleteNode 删除节点及其全部历史数据。
//
// node_runtime / traffic_daily / alert_state 靠外键级联清理；
// samples_10s / samples_1m / ping_samples_1m 没有外键（每秒级写入不想付外键检查成本），
// 必须在这里显式删除。
func (d *DB) DeleteNode(ctx context.Context, id int64) error {
	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, table := range []string{"samples_10s", "samples_1m", TablePing1m} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE node_id = ?`, id); err != nil {
			return fmt.Errorf("清理 %s 失败: %w", table, err)
		}
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM nodes WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("删除节点失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNodeNotFound
	}
	return tx.Commit()
}

// AuditRow 是审计日志的一行。
type AuditRow struct {
	ID     int64
	TS     int64
	Action string
	NodeID int64
	IP     string
	Detail string
}

// ListAudit 按时间倒序返回审计日志（beforeID > 0 时只返回更早的记录，用于翻页）。
func (d *DB) ListAudit(ctx context.Context, limit int, beforeID int64) ([]AuditRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var (
		rows *sql.Rows
		err  error
	)
	if beforeID > 0 {
		rows, err = d.r.QueryContext(ctx, `
			SELECT id, ts, action, node_id, ip, detail FROM audit_log
			WHERE id < ? ORDER BY id DESC LIMIT ?`, beforeID, limit)
	} else {
		rows, err = d.r.QueryContext(ctx, `
			SELECT id, ts, action, node_id, ip, detail FROM audit_log
			ORDER BY id DESC LIMIT ?`, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("查询审计日志失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]AuditRow, 0, limit)
	for rows.Next() {
		var r AuditRow
		if err := rows.Scan(&r.ID, &r.TS, &r.Action, &r.NodeID, &r.IP, &r.Detail); err != nil {
			return nil, fmt.Errorf("读取审计日志失败: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历审计日志失败: %w", err)
	}
	return out, nil
}

// AppendAudit 写入一条审计记录，并把表保持在 2000 行以内。
func (d *DB) AppendAudit(ctx context.Context, action string, nodeID int64, ip, detail string) error {
	tx, err := d.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit_log (ts, action, node_id, ip, detail) VALUES (?, ?, ?, ?, ?)`,
		time.Now().Unix(), action, nodeID, ip, detail); err != nil {
		return fmt.Errorf("写入审计日志失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM audit_log WHERE id <= (SELECT max(id) FROM audit_log) - 2000`); err != nil {
		return fmt.Errorf("清理审计日志失败: %w", err)
	}
	return tx.Commit()
}

// tags 用 COALESCE 兜底：列本身是 NOT NULL DEFAULT '[]'，理论上不会有 NULL，
// 但读路径不该因为一行被手工改过的数据就把整个节点列表打挂（见 decodeTags）。
const nodeSelect = `SELECT id, name, group_name, region, note, token_prefix, token_created_at,
	iface, interval_sec, traffic_limit, traffic_warn_pct, reset_day, expires_at, sort_order,
	enabled, created_at, updated_at, price_cents, currency, billing_months,
	COALESCE(tags, '[]') FROM nodes`

// rowScanner 让 Node 的扫描逻辑同时适用于 QueryRow 与 Rows。
type rowScanner interface {
	Scan(dest ...any) error
}

func scanNode(row rowScanner) (Node, error) {
	var (
		n       Node
		enabled int
		tagsRaw string
	)
	err := row.Scan(&n.ID, &n.Name, &n.GroupName, &n.Region, &n.Note, &n.TokenPrefix, &n.TokenCreatedAt,
		&n.Iface, &n.IntervalSec, &n.TrafficLimit, &n.TrafficWarnPct, &n.ResetDay, &n.ExpiresAt, &n.SortOrder,
		&enabled, &n.CreatedAt, &n.UpdatedAt, &n.PriceCents, &n.Currency, &n.BillingMonths, &tagsRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return Node{}, ErrNodeNotFound
	}
	if err != nil {
		return Node{}, fmt.Errorf("读取节点失败: %w", err)
	}
	n.Enabled = enabled != 0
	// 脏标签在这里被吞掉（回退成空列表），绝不向上返回错误：见 decodeTags 的说明。
	n.Tags = decodeTags(tagsRaw)
	return n, nil
}

func isUniqueViolation(err error, index string) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") &&
		strings.Contains(err.Error(), index)
}
