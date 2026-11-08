package gameserver

import (
	"context"
	"crypto/md5"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"rf4server/internal/proto"
	"rf4server/internal/store"
	"rf4server/internal/wiregen"
)

// 钓鱼经济域：鱼市 20/x、工坊修理 21/x、钓深 17/12、耐久衰减和蓝冠鱼命名。
func init() {
	RegisterExtra(registerEconMarket)
}

func registerEconMarket(d *Dispatch) {
	d.Register(20, 1, hMarketRate)
	d.Register(20, 2, hMarketSell)
	d.Register(20, 3, hMarketLots)
	d.Register(20, 4, hMarketSellOffer)
	d.Register(21, 1, hWorkshopList)
	d.Register(21, 2, hWorkshopDiag)
	d.Register(21, 3, hWorkshopOrder) // 官方语义：订购/更换零件（含修理）
	d.Register(21, 4, hWorkshopOrder) // 官方金币通道同样是换件结构
	d.Register(17, 12, hRodSetDepth)
	d.Register(17, 13, hRodSetClip)
	d.Register(17, 14, hRodSetDrag)
	d.Register(17, 15, hRodSetSpeed)
}

// ---------------------------------------------------------------------------
// 鱼市计价
// ---------------------------------------------------------------------------

// silverRateSeedDivisor 将价值表中的第二列换算为银币/克。
// 鱼市报价和 14/4 渔获展示共用同一除数。
const silverRateSeedDivisor = 28.0

// g1MarketRatio 是 1 档鱼按同鱼种 2 档价格折算时使用的比例。
const g1MarketRatio = 0.45

// marketRateFallback 返回未标定鱼种的银币/克速率。
func marketRateFallback(grade int64) float64 {
	switch grade {
	case 1:
		return 0.05
	case 2:
		return 0.10
	case 3:
		return 0.30
	case 4:
		return 0.50
	}
	return 0.1
}

// freshnessTiers 定义入护时长对应的新鲜度系数。
var freshnessTiers = []struct {
	cutoffHours float64
	factor      float64
}{
	{24.0, 1.0}, {48.0, 0.8}, {72.0, 0.6}, {math.Inf(1), 0.4},
}

// fishRotSeconds 是鱼类腐败阈值，单位为秒。
const fishRotSeconds = 86400.0

// freshnessFactor 新鲜度系数（新鱼 1.0；阶梯衰减，floor 0.4）。
func freshnessFactor(tsCaught, now float64) float64 {
	if tsCaught <= 0 {
		return 1.0
	}
	ageH := (now - tsCaught) / 3600.0
	if ageH < 0 {
		ageH = 0
	}
	for _, t := range freshnessTiers {
		if ageH <= t.cutoffHours {
			return t.factor
		}
	}
	return freshnessTiers[len(freshnessTiers)-1].factor
}

// isFishRotten 判断鱼是否已过腐败阈值。20/1 的 f2 必须反映该状态。
func isFishRotten(tsCaught, now float64) bool {
	if tsCaught <= 0 {
		return false
	}
	return now-tsCaught > fishRotSeconds
}

//go:embed assets/fish_market_prices.json
var fishMarketPricesJSON []byte

//go:embed assets/map_market_multipliers.json
var mapMarketMultipliersJSON []byte

//go:embed assets/map_price_multipliers.json
var mapPriceMultipliersJSON []byte

//go:embed assets/shop_repair_defs.json
var shopRepairDefsJSON []byte

// marketRatePerG 返回鱼市基础收购价（银币/克）。
// 优先使用鱼种和档位速率，1 档可回退到 2 档比例，未标定鱼种使用默认速率。
// 水域覆盖价由 marketSilverPerKgOverlay 单独处理。
func marketRatePerG(species string, grade int64) float64 {
	if grade < 1 {
		grade = 1
	}
	if rate, ok := valueRates[vgKey{species, grade}]; ok && rate[1] > 0 {
		return rate[1] / silverRateSeedDivisor
	}
	if grade == 1 {
		if g2, ok := valueRates[vgKey{species, 2}]; ok && g2[1] > 0 {
			return math.Round(g2[1]*g1MarketRatio/silverRateSeedDivisor*1e6) / 1e6
		}
	}
	return marketRateFallback(grade) / silverRateSeedDivisor
}

// marketSilverPerKgOverlay 返回水域、鱼种和档位对应的银币/公斤覆盖价。
// 目标水域的表值优先；缺失时按其他水域的表值和倍率折算。
// 无可用数据或倍率未登记时返回 0，调用方回退到基础速率。
func marketSilverPerKgOverlay(mapKey, fishKey string, grade int64) float64 {
	overlayOnce.Do(loadMarketOverlay)
	mapKey = normalizeMarketMapKey(mapKey)
	if marketPriceTable == nil || len(marketPriceTable) == 0 {
		return 0
	}
	if mapKey == "" || fishKey == "" {
		return 0
	}
	pct := marketMapPct(mapKey)
	if pct <= 0 {
		return 0
	}
	g := strconv.FormatInt(grade, 10)
	if g == "0" || g == "-" {
		g = "1"
	}
	// 目标水域自己的表优先（避免被别的表的同种鱼顶掉）
	order := []string{mapKey}
	for k := range marketPriceTable {
		if k != mapKey {
			order = append(order, k)
		}
	}
	for _, baseMap := range order {
		rec := marketPriceTable[baseMap][fishKey]
		if rec == nil {
			continue
		}
		v, ok := rec[g]
		if !ok || v <= 0 {
			continue
		}
		basePct := marketMapPct(baseMap)
		if basePct <= 0 {
			continue // 缺少折算基准倍率时跳过该表
		}
		return v / basePct * pct
	}
	return 0
}

type marketOverlayMap map[string]map[string]map[string]float64

var (
	overlayOnce      sync.Once
	marketPriceTable marketOverlayMap   // by_map[map][fish][gradeStr] = 银/kg
	marketPctTable   map[string]float64 // by_map_key[map] = 倍率
)

func loadMarketOverlay() {
	var prices struct {
		ByMap map[string]map[string]map[string]float64 `json:"by_map"`
	}
	if err := json.Unmarshal(fishMarketPricesJSON, &prices); err == nil {
		marketPriceTable = prices.ByMap
	}
	var mults struct {
		ByMapKey map[string]struct {
			MarketPct float64 `json:"market_pct"`
		} `json:"by_map_key"`
	}
	if err := json.Unmarshal(mapMarketMultipliersJSON, &mults); err == nil {
		marketPctTable = map[string]float64{}
		for k, rec := range mults.ByMapKey {
			marketPctTable[k] = rec.MarketPct
		}
	}
}

// normalizeMarketMapKey 把客户端的鱼市场景键（如 level_004_fishmarket）归一到
// 价格表中的地图键（level_004_torfyanoe_ozero），否则覆盖价查不到会回退基础速率。
func normalizeMarketMapKey(mapKey string) string {
	if _, ok := marketPctTable[mapKey]; ok {
		return mapKey
	}
	// 形如 level_004_fishmarket：地图键在前，场景后缀在后。
	if strings.HasSuffix(mapKey, "_fishmarket") {
		base := strings.TrimSuffix(mapKey, "_fishmarket")
		for m := range marketPctTable {
			if m == base || strings.HasPrefix(m, base+"_") {
				return m
			}
		}
		return mapKey
	}
	// 形如 fishmarket_012：编号在前，映射到 level_012_* 的地图键。
	if strings.HasPrefix(mapKey, "fishmarket_") {
		num := strings.TrimPrefix(mapKey, "fishmarket_")
		for m := range marketPctTable {
			if strings.HasPrefix(m, "level_"+num+"_") {
				return m
			}
		}
	}
	return mapKey
}

// marketMapPct 返回水域鱼市价格倍率；未登记时返回 0。
func marketMapPct(mapKey string) float64 {
	overlayOnce.Do(loadMarketOverlay)
	return marketPctTable[mapKey]
}

// marketPriceCentsForMap 计算单条鱼的鱼市报价（银币分）。
// 优先使用水域覆盖价，未命中时使用基础速率，两者都乘以新鲜度。
// marketDayForced 允许用环境变量 RF4_MARKET_DAY 覆盖鱼市日期（yyyyMMdd），
// 仅用于验证每日价格波动；空值时使用真实 UTC 日期。读取发生在包初始化，
// 计价路径不做文件 I/O。
var marketDayForced = os.Getenv("RF4_MARKET_DAY")

func marketDay(now float64) string {
	if marketDayForced != "" {
		return marketDayForced
	}
	return time.Unix(int64(now), 0).UTC().Format("20060102")
}

// dailyMarketFactor 返回按 UTC 日期与鱼种确定的每日价格系数，范围 0.95–1.05。
// 同一鱼种在同一天内稳定，跨天重新取值，用于模拟鱼市日价波动。
func dailyMarketFactor(species string, now float64) float64 {
	day := marketDay(now)
	h := fnv.New32a()
	_, _ = h.Write([]byte(day))
	_, _ = h.Write([]byte("|"))
	_, _ = h.Write([]byte(species))
	r := float64(h.Sum32()%10000) / 9999.0
	return 0.95 + 0.10*r
}

func marketPriceCentsForMap(e *store.KeepnetEntry, now float64, mapKey string) int64 {
	w := e.WeightG
	if w < 1 {
		w = 1
	}
	grade := e.Cat
	if grade < 1 {
		grade = 1
	}
	fresh := freshnessFactor(e.TS, now)
	factor := dailyMarketFactor(e.Species, now)
	if perKg := marketSilverPerKgOverlay(mapKey, e.Species, grade); perKg > 0 {
		return int64(math.Max(1, math.Round(perKg*factor*(float64(w)/1000.0)*100*fresh)))
	}
	return int64(math.Max(1, math.Round(marketRatePerG(e.Species, grade)*factor*float64(w)*100*fresh)))
}

// marketPriceCents 在未指定地图时计算报价，地图路径应使用 marketPriceCentsForMap。
func marketPriceCents(e *store.KeepnetEntry, now float64) int64 {
	return marketPriceCentsForMap(e, now, "")
}

// levelKeyOfLoc 将地点名解析为鱼市价格表使用的水域键。
func (s *Server) levelKeyOfLoc(ctx context.Context, uid int64, loc string) string {
	loc = strings.TrimSpace(loc)
	if strings.HasPrefix(loc, "level_") {
		return loc
	}
	if s.DB != nil {
		if lk := s.DB.ResolveLevelKey(ctx, loc); lk != "" {
			return lk
		}
	}
	if strings.HasPrefix(loc, "lvl2") {
		return "level_002_kirzah_river"
	}
	if strings.HasPrefix(loc, "loc1") {
		return "level_001_lake"
	}
	return s.currentLevel(uid)
}

// ---------------------------------------------------------------------------
// 20/1 鱼市询价
// ---------------------------------------------------------------------------

// hMarketRate 处理 20/1：逐条返回 302 报价，包含鱼 GUID、银币分和腐败标志。
// 未识别的 GUID 和已挂单的鱼会被跳过。
func hMarketRate(ctx *Ctx) ([]byte, bool) {
	srv := ctx.Sess.Srv
	if !srv.requireDB(ctx) {
		return nil, false
	}
	uid := ctx.Sess.Userid
	cctx := ctx.Sess.pushCtx()
	now := float64(time.Now().Unix())
	out := proto.Args(proto.S("111"))
	rotten := 0
	locked := srv.openMarketOfferFishSet(cctx, uid)
	for _, g := range deepGUIDs(ctx.Tree) {
		if locked[g] {
			continue
		}
		e, err := srv.DB.KeepnetGet(cctx, uid, g)
		if err != nil || e == nil {
			continue
		}
		flag := int64(0)
		if isFishRotten(e.TS, now) {
			flag = 1
			rotten++
		}
		mapKey := srv.levelKeyOfLoc(cctx, uid, e.KeepLoc)
		price := marketPriceCentsForMap(e, now, mapKey)
		out.Items = append(out.Items, wiregen.MustMakeObj("jpmlmnfkbfn", map[int]*proto.Value{
			0: proto.Guid(g),
			1: proto.U64(uint64(price)),
			2: proto.U32(uint32(flag)),
		}))
	}
	ctx.Log.Info("20/1 询价", "条数", len(out.Items), "腐败", rotten)
	body, err := proto.EncodeValue(out, ctx.Sess.Table())
	if err != nil {
		return nil, false
	}
	return body, true
}

// ---------------------------------------------------------------------------
// 20/2 卖鱼（银币只在此处入账）
// ---------------------------------------------------------------------------

// hMarketSell 处理 20/2：移除护网鱼、增加银币并写流水，随后推送 2/1 和 4/21，
// 最后返回 303 明细。
func hMarketSell(ctx *Ctx) ([]byte, bool) {
	srv := ctx.Sess.Srv
	if !srv.requireDB(ctx) {
		return nil, false
	}
	uid := ctx.Sess.Userid
	cctx := ctx.Sess.pushCtx()
	now := float64(time.Now().Unix())
	guids := uniqueStrings(deepGUIDs(ctx.Tree))

	// 地图解析使用连接池，必须在事务前完成；事务内只重读护网行并核价。
	levels := map[string]string{}
	if list, err := srv.DB.KeepnetList(cctx, uid); err == nil {
		for _, e := range list {
			levels[e.EchoGUID] = srv.levelKeyOfLoc(cctx, uid, e.KeepLoc)
		}
	}

	type soldPair struct {
		guid  string
		price int64
	}
	var sold []soldPair
	tx, err := beginEconomyTx(cctx, srv.DB)
	if err != nil {
		ctx.Log.Warn("20/2 开启事务失败", "err", err)
		return []byte{0x0F}, true
	}
	defer func() { _ = tx.Rollback() }()

	removed := make([]string, 0, len(guids))
	total := int64(0)
	for _, g := range guids {
		e := &store.KeepnetEntry{}
		err := tx.QueryRowContext(cctx, `
			SELECT echo_guid, uid, COALESCE(species,''), COALESCE(name_zh,''), weight_g, length_m,
			       cat, COALESCE(keep_loc,''), COALESCE(keep_spot,0), COALESCE(keep_bait,''),
			       COALESCE(keep_level,''), COALESCE(ts,0)
			FROM keepnet_items WHERE uid=? AND echo_guid=?`, uid, g).Scan(
			&e.EchoGUID, &e.UID, &e.Species, &e.NameZh, &e.WeightG, &e.LengthM,
			&e.Cat, &e.KeepLoc, &e.KeepSpot, &e.KeepBait, &e.KeepLevel, &e.TS)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			ctx.Log.Warn("20/2 读取护网失败", "guid", g, "err", err)
			return []byte{0x0F}, true
		}
		var locked int
		err = tx.QueryRowContext(cctx, `
			SELECT 1 FROM market_offer WHERE fish_guid=? AND status='open' LIMIT 1`, g).Scan(&locked)
		if err == nil {
			ctx.Log.Warn("20/2 拒绝出售已挂单鱼", "guid", g)
			return []byte{0x0F}, true
		}
		if err != sql.ErrNoRows {
			ctx.Log.Warn("20/2 读取挂单锁失败", "guid", g, "err", err)
			return []byte{0x0F}, true
		}
		mapKey := levels[g]
		if mapKey == "" && e.KeepLevel != "" {
			mapKey = e.KeepLevel
		}
		price := marketPriceCentsForMap(e, now, mapKey)
		sold = append(sold, soldPair{guid: g, price: price})
		total += price
		removed = append(removed, g)
	}
	if len(sold) > 0 {
		if err := applyWalletTx(cctx, tx, uid, total, 0, "sell_fish", "", "",
			fmt.Sprintf("%d fish", len(sold))); err != nil {
			ctx.Log.Warn("20/2 入账失败", "err", err)
			return []byte{0x0F}, true
		}
		for _, g := range removed {
			deleted, derr := srv.DB.DeleteKeepnetTx(cctx, tx, uid, g)
			if derr != nil {
				ctx.Log.Warn("20/2 移除护网失败", "guid", g, "err", derr)
				return []byte{0x0F}, true
			}
			if !deleted {
				return []byte{0x0F}, true
			}
		}
	}
	if err := tx.Commit(); err != nil {
		ctx.Log.Warn("20/2 提交失败", "err", err)
		return []byte{0x0F}, true
	}

	if len(sold) > 0 {
		st := srv.stateFor(uid)
		st.mu.Lock()
		kept := st.Keepnet[:0]
		for _, k := range st.Keepnet {
			if !containsGUID(removed, k.FishGUID) {
				kept = append(kept, k)
			}
		}
		st.Keepnet = kept
		st.mu.Unlock()
		ctx.Sess.Push2_1()
		ctx.Sess.Push4_21(removed)
		srv.retryCompletedCatchRecovery(ctx.Sess)
		ctx.Log.Info("20/2 卖出", "条数", len(sold), "银分", total)
	}
	out := proto.Args(proto.S("112"))
	for _, sp := range sold {
		out.Items = append(out.Items, wiregen.MustMakeObj("iajmdfbjjel", map[int]*proto.Value{
			0: proto.Guid(sp.guid),
			1: proto.U64(uint64(sp.price)),
			2: proto.U32(0),
		}))
	}
	body, err := proto.EncodeValue(out, ctx.Sess.Table())
	if err != nil {
		return nil, false
	}
	return body, true
}

// ---------------------------------------------------------------------------
// 20/3 鱼市分组列表
// ---------------------------------------------------------------------------

// hMarketLots 20/3 返回独立的咖啡厅收购需求。
func hMarketLots(ctx *Ctx) ([]byte, bool) { return hCafeLots(ctx) }

// ---------------------------------------------------------------------------
// 20/4 挂单出售
// ---------------------------------------------------------------------------

// openMarketOfferFishSet 返回当前玩家处于 open 挂单中的鱼 GUID 集合。
// 普通鱼市和咖啡厅都必须拒绝出售已锁定的鱼。
func (s *Server) openMarketOfferFishSet(ctx context.Context, uid int64) map[string]bool {
	out := map[string]bool{}
	if s.DB == nil {
		return out
	}
	offers, err := s.DB.ListMarketOffers(ctx, store.MarketOfferFilter{
		SellerUID: uid, Status: store.MarketOfferOpen, Limit: 1000,
	})
	if err != nil {
		s.log.Warn("读取鱼市挂单锁失败", "uid", uid, "err", err)
		return out
	}
	for _, offer := range offers {
		out[offer.FishGUID] = true
	}
	return out
}

func encodeMarketSellOfferResult(ctx *Ctx, priceCents int64, status uint32) ([]byte, bool) {
	obj := wiregen.MustMakeObj("imhmlbejglk", map[int]*proto.Value{
		0: proto.U64(uint64(priceCents)),
		1: proto.U32(status),
	})
	body, err := proto.EncodeValue(obj, ctx.Sess.Table())
	if err != nil {
		return nil, false
	}
	return body, true
}

// hMarketSellOffer 20/4 交付咖啡厅订单，立即扣鱼并支付银币。
func hMarketSellOffer(ctx *Ctx) ([]byte, bool) { return hCafeSubmit(ctx) }

// ---------------------------------------------------------------------------
// 工坊 21/x
// ---------------------------------------------------------------------------

// 修理费用的基础值（银币分和金币）。
const (
	repairBaseCents    = 200
	repairPerSlotCents = 100
	repairGoldBase     = 1
)

// 修理倍率资产。shop_repair_defs.json 只保存修理店索引；
// 未知店铺使用当前地图倍率，未知地图回退到 1.0。
type repairMapMultiplierRecord struct {
	Repair       float64 `json:"repair"`
	RepairBeluga float64 `json:"repair_beluga"`
	RepairZeiman float64 `json:"repair_zeiman"`
}

type repairStoreDefinition struct {
	Brands []string `json:"brands"`
	MapKey string   `json:"map_key"`
}

var (
	repairMultiplierOnce sync.Once
	repairMapMultipliers map[string]repairMapMultiplierRecord
	repairStoreDefs      map[string]repairStoreDefinition
)

func loadRepairMultipliers() {
	var prices struct {
		ByMapKey map[string]repairMapMultiplierRecord `json:"by_map_key"`
	}
	if err := json.Unmarshal(mapPriceMultipliersJSON, &prices); err != nil {
		repairMapMultipliers = map[string]repairMapMultiplierRecord{}
	} else {
		repairMapMultipliers = prices.ByMapKey
	}
	var shops struct {
		Stores map[string]repairStoreDefinition `json:"stores"`
	}
	if err := json.Unmarshal(shopRepairDefsJSON, &shops); err != nil {
		repairStoreDefs = map[string]repairStoreDefinition{}
	} else {
		repairStoreDefs = shops.Stores
	}
}

func positiveRepairMultiplier(v float64) (float64, bool) {
	if v > 0 {
		return v, true
	}
	return 0, false
}

// repairMultForMap 返回地图修理倍率。Beluga/Zeiman 品牌列优先，
// 缺列时使用地图通用值，缺数据或非法值回退到 1.0。
func repairMultForMap(mapKey string, brands []string) float64 {
	repairMultiplierOnce.Do(loadRepairMultipliers)
	rec, ok := repairMapMultipliers[mapKey]
	if !ok {
		return 1.0
	}
	brandSet := make(map[string]bool, len(brands))
	for _, brand := range brands {
		brandSet[strings.ToLower(strings.TrimSpace(brand))] = true
	}
	if brandSet["beluga"] {
		if v, ok := positiveRepairMultiplier(rec.RepairBeluga); ok {
			return v
		}
	}
	if brandSet["zeiman"] {
		if v, ok := positiveRepairMultiplier(rec.RepairZeiman); ok {
			return v
		}
	}
	if v, ok := positiveRepairMultiplier(rec.Repair); ok {
		return v
	}
	return 1.0
}

// repairMultiplierForStore 通过店铺索引解析地图和品牌；店铺未知时使用当前地图。
func repairMultiplierForStore(storeID, fallbackMapKey string) float64 {
	repairMultiplierOnce.Do(loadRepairMultipliers)
	mapKey := fallbackMapKey
	var brands []string
	if def, ok := repairStoreDefs[strings.TrimSpace(storeID)]; ok {
		if def.MapKey != "" {
			mapKey = def.MapKey
		}
		brands = def.Brands
	}
	return repairMultForMap(mapKey, brands)
}

// repairCost 根据槽位数、磨损和地图倍率计算修理费用，磨损因子下限为 25%。
// 全新物品由调用方直接放行，不进入此计算。
func repairCost(slots int, useGold bool, wear, mapMult float64) int64 {
	n := slots
	if n < 1 {
		n = 1
	}
	if wear < 0 {
		wear = 0
	} else if wear > 1 {
		wear = 1
	}
	factor := math.Max(0.25, wear)
	if mapMult <= 0 {
		mapMult = 1.0
	}
	if useGold {
		return int64(math.Max(1, math.Round(repairGoldBase*factor*mapMult)))
	}
	return int64(math.Round(float64(repairBaseCents+repairPerSlotCents*n) * factor * mapMult))
}

// hWorkshopList 处理 21/1，返回修理面板所需的物品清单。
//
// 必须覆盖玩家的全部随身物品：客户端修理页选中的物品要在本响应里按 GUID 查
// 修理信息，查不到就拿不到模型（详情空白）。满耐久物品同样返回，磨损记 0。
func hWorkshopList(ctx *Ctx) ([]byte, bool) {
	srv := ctx.Sess.Srv
	if !srv.requireDB(ctx) {
		return nil, false
	}
	cctx := ctx.Sess.pushCtx()
	rows, err := srv.DB.ListRepairItems(cctx, ctx.Sess.Userid)
	if err != nil {
		rows = nil
	}
	out := proto.Args(proto.S("144"))
	for _, r := range rows {
		parts := srv.repairPartsOf(cctx, r)
		slots := make([]uint32, 0, len(parts))
		for _, p := range parts {
			slots = append(slots, p.Cat)
		}
		// f1 是完好度（1.0=全新）：客户端实测值为 0.9999x~1.0，不是磨损百分比，
		// 发 1-durability 会把满耐久物品报成 0% 完好度，修理报价随之算错。
		// f2 是可修档位（客户端实测 4/5 量级），这里取可修零件数。
		condition := r.Durability
		grade := int64(len(parts))
		out.Items = append(out.Items, wiregen.MustMakeObj("ookkpknacja", map[int]*proto.Value{
			0: proto.Guid(r.GUID),
			1: proto.U32(proto.F32Bits(float32(condition))),
			2: proto.U32(uint32(grade)),
			3: proto.U32Arr(slots),
		}))
	}
	body, err := proto.EncodeValue(out, ctx.Sess.Table())
	if err != nil {
		return nil, false
	}
	return body, true
}

// repairPart 是修理面板要展示的一个可修零件。
type repairPart struct {
	GUID string
	NID  uint32  // 零件定义 numeric_id（内嵌子件，item 表里没有行）
	Cat  uint32  // oinpmjophkh 物品品类（客户端按类别渲染零件行）
	Cond float64 // 完好度 0..1，1=全新；只影响报价，不影响是否展示
}

// repairPartsOf 从物品树里收集可修零件：
//   - f0..f2 出厂部件（竿柄/导环等，内嵌子对象，带自己的 guid 和耐久）；
//   - f3 挂线、f4 挂轮（存的是子件 guid）；
//   - f5 装配列表（gcfcdnfmnbh：品类 + guid + 附加值）。
//
// 修理面板的零件行和报价都基于这份清单；只查 rig_component 派生表会因为
// 历史数据未回填而永远为空。
func (s *Server) repairPartsOf(ctx context.Context, it *store.Item) []repairPart {
	if it == nil {
		return nil
	}
	fields, err := FieldsFromJSON(it.FieldsJSON)
	if err != nil {
		return nil
	}
	var out []repairPart
	seen := map[string]bool{}
	add := func(guid string, nid, cat uint32, cond float64) {
		if guid == "" || seen[guid] {
			return
		}
		seen[guid] = true
		out = append(out, repairPart{GUID: guid, NID: nid, Cat: cat, Cond: cond})
	}
	catOf := func(numericID uint32) uint32 {
		if def, derr := s.DB.GetItemDef(ctx, int64(numericID)); derr == nil && def != nil {
			return uint32(def.Category)
		}
		return 0
	}
	condOf := func(v *proto.Value) float64 {
		if v == nil || v.Kind != proto.KindObj {
			return 1.0
		}
		// 子件耐久字段按类名查（竿件在下标 15，渔轮件在下标 6），值以 f32 位形存放。
		if idx := fieldIdxByCS(v.Class, csDurability); idx >= 0 && idx < v.FieldCount() {
			if f := v.Field(idx); f.Num != nil {
				return float64(math.Float32frombits(f.U32()))
			}
		}
		return 1.0
	}
	// 遍历所有 VAL 对象槽：竿的出厂部件在 f0~f2，渔线轮的组件在 f2~f14
	// （线杯/轴承/摇把/油/摩擦片/曳力片…），统一按“有 guid 的子件”收集。
	for i := 0; i < len(fields); i++ {
		if fields[i].Op != "VAL" {
			continue
		}
		child := fields[i].Val
		if child == nil || child.Kind != proto.KindObj {
			continue
		}
		gidx := fieldIdxByCS(child.Class, csGUID)
		if gidx < 0 || gidx >= child.FieldCount() {
			continue
		}
		g := child.Fields[gidx].Hex
		if g == "" {
			continue
		}
		var nid, cat uint32
		if nidx := fieldIdxByCS(child.Class, csNumericID); nidx >= 0 && nidx < child.FieldCount() {
			nid = uint32(u64Of(child.Fields[nidx].Num))
			cat = catOf(nid)
		}
		// 只收零件本身：摇柄(34) 不进工坊列表；线(11)/渔轮(6)/拟饵等
		// 属于已装配的渔具，不是这根杆子的零件，一律排除。
		if !workshopPartCategories[cat] {
			continue
		}
		add(g, nid, cat, condOf(child))
	}
	return out
}

// ---- 工坊零件定价 / 替代件 / 订购 ----

// 零件价格缓存：item_price 是全局价目表，零件本身通常没有单独定价。
var partPriceOnce sync.Once
var partPriceByItemID map[string][2]int64

// 零件没有独立价目时按品类给基准价，数值参考官方抓包样本
// （轴承 845/8、摩擦片 754/7、齿轮 296/5、竿坯 1200/12 …）。
// workshopPartCategories 允许出现在工坊零件列表里的品类。
// 4 竿柄 / 5 导环 / 7 轴承 / 8 线杯 / 9 摩擦片 / 10 轮油 /
// 34 摇柄 / 36 竿坯 / 52 齿轮 / 63 电机 / 81 引擎
var workshopPartCategories = map[uint32]bool{
	4: true, 5: true, 7: true, 8: true, 9: true, 10: true,
	34: true, 36: true, 52: true, 63: true, 81: true,
}

// noReplaceCategories 不允许更换型号、只能修复的零件品类。
// 轴承和线杯在精确兼容表中有合法替代件，不能在这里硬禁用。
var noReplaceCategories = map[int64]bool{5: true, 36: true}

var partBasePrice = map[int64][2]int64{
	4:  {600, 6},   // 竿柄
	5:  {400, 4},   // 导环
	7:  {845, 8},   // 轴承
	8:  {900, 9},   // 线杯
	9:  {754, 7},   // 摩擦片
	10: {50, 1},    // 轮油
	34: {300, 3},   // 摇把
	36: {1200, 12}, // 竿坯
	52: {296, 5},   // 齿轮
}

// partDefCache 缓存 item_id -> 定义，供按属性定价使用。
var partDefMu sync.Mutex
var partDefCache = map[string]*store.ItemDef{}

func (s *Server) defByItemID(ctx context.Context, itemID string) *store.ItemDef {
	partDefMu.Lock()
	d, ok := partDefCache[itemID]
	partDefMu.Unlock()
	if ok {
		return d
	}
	def, err := s.DB.GetItemDefByItemID(ctx, itemID)
	if err != nil {
		def = nil
	}
	partDefMu.Lock()
	partDefCache[itemID] = def
	partDefMu.Unlock()
	return def
}

// partPrice 按零件属性推导价格（item_price 里没有零件价）。
//
// 依据官方样本标定：摩擦片 ≈ 制动力(kg)×140、齿轮 ≈ 传动比×48、
// 轴承 ≈ 品质×10、线杯 ≈ 容量×12；金币按银币/110 取整。
func (s *Server) partPrice(ctx context.Context, itemID string, category int64) (int64, int64) {
	partPriceOnce.Do(func() {
		partPriceByItemID = map[string][2]int64{}
		rows, err := s.DB.SQL.QueryContext(context.Background(),
			`SELECT item_id, COALESCE(silver,0), COALESCE(gold,0) FROM item_price`)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var silver, gold int64
			if err := rows.Scan(&id, &silver, &gold); err != nil {
				continue
			}
			partPriceByItemID[id] = [2]int64{silver, gold}
		}
	})
	if p, ok := partPriceByItemID[itemID]; ok && (p[0] > 0 || p[1] > 0) {
		return p[0], p[1]
	}
	def := s.defByItemID(ctx, itemID)
	var silver float64
	if def != nil {
		attrs := defAttrs(def)
		num := func(k string) (float64, bool) {
			if attrs == nil {
				return 0, false
			}
			switch v := attrs[k].(type) {
			case float64:
				return v, true
			case int64:
				return float64(v), true
			}
			return 0, false
		}
		switch def.Category {
		case 9: // 摩擦片：制动力，价格按平方放大（26 × kg²，5.4kg≈758 银）
			if b, ok := num("omghfmdfjpc"); ok {
				d := float64(proto.F32FromBits(uint32(b)))
				silver = 26 * d * d
			}
		case 7: // 轴承：品质百分比
			if q, ok := num("obifdihmcfn"); ok {
				silver = q * 10
			}
		case 8: // 线杯：容量
			if c, ok := num("boejfeemeej"); ok {
				silver = c * 12
			}
		case 52: // 齿轮：传动比
			if b, ok := num("dpgbopbanal"); ok {
				silver = float64(proto.F32FromBits(uint32(b))) * 480
			}
		case 10: // 润滑油
			silver = 50
		}
	}
	if silver <= 0 {
		if p, ok := partBasePrice[category]; ok {
			silver = float64(p[0])
		} else {
			silver = 500
		}
	}
	return int64(math.Round(silver)), int64(math.Round(silver / 110))
}

func priceObj(silver, gold int64) *proto.Value {
	return wiregen.MustMakeObj("oifghenlghh", map[int]*proto.Value{
		0: proto.U64(uint64(silver)),
		1: proto.U64(uint64(gold)),
	})
}

// partAlternatives 构造某零件的可替换件工单列表（官方 21/2 的 f5，hpopgbjohpk[]）。
//
// 官方每条工单：{新件 systemID, 旧件折抵价, 新件价格, 工时(分钟)}。
// 兼容件来源：同品类（官方 UI 上"有 N 种可选的变体"就是这里的条目数）。
// compatParts 读取 data/compat_parts.json 的精确兼容表：
// {"轮子systemID": {"9": ["reef_fb_cnvEG70", ...]}}
// 表里有的按表发；没有的回退到"同命名族"近似。
var compatOnce sync.Once
var compatTable map[string]map[string][]string

// compatTablePath 依次尝试多个位置，避免服务端工作目录不同导致读不到。
func compatTablePath() (string, bool) {
	var cands []string
	if p := os.Getenv("RF4_COMPAT_FILE"); p != "" {
		cands = append(cands, p)
	}
	cands = append(cands,
		"data/compat_parts.json", "server_data/compat_parts.json",
		"../server_data/compat_parts.json", "../../server_data/compat_parts.json")
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		cands = append(cands,
			filepath.Join(dir, "data", "compat_parts.json"),
			filepath.Join(dir, "..", "data", "compat_parts.json"))
	}
	for _, p := range cands {
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

// LoadCompatTable 启动时加载精确兼容表，返回条目数与来源路径。
func (s *Server) LoadCompatTable() (int, string, error) {
	compatOnce.Do(func() {})
	compatTable = map[string]map[string][]string{}
	path, ok := compatTablePath()
	if !ok {
		return 0, "", os.ErrNotExist
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, path, err
	}
	if err := json.Unmarshal(raw, &compatTable); err != nil {
		return 0, path, err
	}
	n := 0
	for _, byCat := range compatTable {
		for _, ids := range byCat {
			n += len(ids)
		}
	}
	return n, path, nil
}

// compatFor 返回该轮子该品类的替代型号。
// found=true 表示表里有这台轮子（以表为准，缺的品类即"没有替代品"）；
// found=false 才回退到命名前缀近似。
func (s *Server) compatFor(reelItemID, category string) ([]string, bool) {
	compatOnce.Do(func() {
		if compatTable == nil {
			compatTable = map[string]map[string][]string{}
		}
	})
	byCat, ok := compatTable[reelItemID]
	if !ok {
		return nil, false
	}
	return byCat[category], true
}

func (s *Server) partAlternatives(ctx context.Context, reelItemID string, part repairPart) []*proto.Value {
	nid := int64(part.NID)
	if nid == 0 {
		if row, err := s.DB.GetItemByGUID(ctx, part.GUID); err == nil && row != nil {
			nid = row.NumericID
		}
	}
	if nid == 0 {
		return nil
	}
	def, derr := s.DB.GetItemDef(ctx, nid)
	if derr != nil || def == nil || def.ItemID == "" {
		return nil
	}
	out := make([]*proto.Value, 0, 32)
	seen := map[string]bool{}
	addJob := func(itemID string) {
		if itemID == "" || seen[itemID] {
			return
		}
		seen[itemID] = true
		silver, gold := s.partPrice(ctx, itemID, def.Category)
		comp := wiregen.MustMakeObj("oifghenlghh", map[int]*proto.Value{
			0: proto.U64(uint64(silver / 10)),
			1: proto.U64(uint64(gold / 10)),
		})
		price := wiregen.MustMakeObj("oifghenlghh", map[int]*proto.Value{
			0: proto.U64(uint64(silver)),
			1: proto.U64(uint64(gold)),
		})
		out = append(out, wiregen.MustMakeObj("hpopgbjohpk", map[int]*proto.Value{
			0: proto.StrVal(itemID),
			1: comp,
			2: price,
			3: proto.U32(2),
		}))
	}
	addJob(def.ItemID)
	// 导环/杆坯只允许修复；轮子的轴承和线杯按精确兼容表提供替代型号。
	if noReplaceCategories[def.Category] {
		return out
	}
	// 轮油是通用保养耗材，不受轮子型号前缀限制。静态库中的各品质油均可
	// 作为替代件，实际品质会进入服务端超载耐久计算。
	if def.Category == 10 {
		rows, qerr := s.DB.SQL.QueryContext(ctx, `
			SELECT item_id FROM item_def WHERE category=10
			ORDER BY CAST(json_extract(attrs_json,'$.obifdihmcfn') AS INTEGER), numeric_id`)
		if qerr == nil {
			var oils []string
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err == nil {
					oils = append(oils, id)
				}
			}
			rows.Close()
			for _, id := range oils {
				addJob(id)
			}
		}
		return out
	}
	if ids, found := s.compatFor(reelItemID, strconv.FormatInt(def.Category, 10)); found {
		for _, id := range ids {
			addJob(id)
		}
		return out
	}
	// 回退：同品类 + 同命名族（取到最后一个 -/_ 的前缀）。
	prefix := def.ItemID
	if k := strings.LastIndexAny(prefix, "-_"); k >= 0 {
		prefix = prefix[:k+1]
	} else {
		prefix = ""
	}
	if prefix != "" {
		rows, qerr := s.DB.SQL.QueryContext(ctx,
			`SELECT item_id FROM item_def WHERE category=? AND item_id LIKE ? ORDER BY numeric_id LIMIT 32`,
			def.Category, prefix+"%")
		if qerr == nil {
			// 先把 id 收完再处理：addJob 里还会查定义/价格，
			// 在结果集未关闭时复用同一连接会死锁。
			var cands []string
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					continue
				}
				cands = append(cands, id)
			}
			rows.Close()
			for _, id := range cands {
				addJob(id)
			}
		}
	}
	return out
}

// childCategory 返回子件实例定义里的品类（oinpmjophkh），取不到返回 -1。
func (s *Server) childCategory(ctx context.Context, v *proto.Value) int64 {
	if v == nil || v.Kind != proto.KindObj {
		return -1
	}
	nidx := fieldIdxByCS(v.Class, csNumericID)
	if nidx < 0 || nidx >= v.FieldCount() {
		return -1
	}
	n := u64Of(v.Fields[nidx].Num)
	if def, err := s.DB.GetItemDef(ctx, int64(n)); err == nil && def != nil {
		return def.Category
	}
	return -1
}

// childSystemID 返回子件定义里的 systemID（item_id），取不到返回空串。
func (s *Server) childSystemID(ctx context.Context, v *proto.Value) string {
	if v == nil || v.Kind != proto.KindObj {
		return ""
	}
	nidx := fieldIdxByCS(v.Class, csNumericID)
	if nidx < 0 || nidx >= v.FieldCount() {
		return ""
	}
	n := u64Of(v.Fields[nidx].Num)
	if def, err := s.DB.GetItemDef(ctx, int64(n)); err == nil && def != nil {
		return def.ItemID
	}
	return ""
}

// buildPartInstance 按 systemID 构造一个装配子件实例（guid 由宿主 guid + key 稳定派生）。
// 与 buildFactoryChild 同源，但不带"竿坯 2048 出厂位"，用于工坊换件。
func (s *Server) buildPartInstance(ctx context.Context, host *store.Item, sysID, key string) (*proto.Value, error) {
	def, err := s.DB.GetItemDefByItemID(ctx, sysID)
	if err != nil || def == nil {
		return nil, err
	}
	cls, ok := categoryWireClass[def.Category]
	if !ok {
		cls = struct {
			Class  string
			TypeID int64
		}{fallbackWireClass, fallbackTypeID}
	}
	sum := md5.Sum([]byte(host.GUID + "|" + key))
	childGUID := fmt.Sprintf("%x", sum)
	obj, err := wiregen.MakeObj(cls.Class, nil)
	if err != nil {
		return nil, err
	}
	setGUID := func() {
		if idx := fieldIdxByCS(cls.Class, csGUID); idx >= 0 {
			obj.Fields[idx].Hex = childGUID
		} else if fieldOpAt(cls.Class, 0) == "GUID16" {
			obj.Fields[0].Hex = childGUID
		}
	}
	setNum := func(cs string, v uint64) {
		if idx := fieldIdxByCS(cls.Class, cs); idx >= 0 {
			obj.Fields[idx].Num = ptrU64(v)
		}
	}
	setFloat := func(cs string, v float64) {
		idx := fieldIdxByCS(cls.Class, cs)
		if idx < 0 {
			return
		}
		if fieldIsFloat(cls.Class, idx) {
			obj.Fields[idx].Float = v
			return
		}
		if fieldOpAt(cls.Class, idx) == "U32" {
			obj.Fields[idx].Num = ptrU64(uint64(math.Float32bits(float32(v))))
		}
	}
	setGUID()
	setNum(csNumericID, uint64(def.NumericID))
	setNum(csSlotEnum, 4)
	s.applyDefAttrs(def, cls.Class, obj)
	base := u64Of(firstNonNil(obj.Fields, fieldIdxByCS(cls.Class, csStateEnum)).Num)
	if base == 0 {
		if defBits, ok := defAttrU64(def, "hgaijieklek"); ok {
			base = defBits
		}
	}
	setFloat(csWorkmanship, host.Workmanship)
	setFloat(csDurability, host.Durability)
	if idx := fieldIdxByCS(cls.Class, csStateEnum); idx >= 0 {
		obj.Fields[idx].Num = ptrU64(base | subMountFlag)
	}
	// 替换摩擦片时必须把定义中的实际制动力写进 147.f0；只写 numeric_id
	// 会让客户端显示更换成功，但收到一个 0kg 摩擦片并一直等待有效模型。
	if def.Category == 9 {
		setFloat(csDragForce, itemDefF32(def, gearAttrDragMax)*host.Workmanship)
	}
	if idx := fieldIdxByCS(cls.Class, csParent); idx >= 0 {
		obj.Fields[idx].Hex = host.GUID
	}
	return obj, nil
}

// workshopSlots 返回物品可修零件的品类列表。
//
// 响应字段 f3 在客户端是 oinpmjophkh[]（普通 u32 数组，marker 0x05）；
// 若改成 typed_u32arr（0x07 带类型名）会解码成 kkeibjcmjnl，
// 客户端强转 oinpmjophkh[] 时抛 InvalidCastException，整行作废。
func workshopSlots(ctx context.Context, srv *Server, guid string) []uint32 {
	row, err := srv.DB.GetItemByGUID(ctx, guid)
	if err != nil || row == nil {
		return nil
	}
	parts := srv.repairPartsOf(ctx, row)
	out := make([]uint32, 0, len(parts))
	for _, p := range parts {
		out = append(out, p.Cat)
	}
	return out
}

// hWorkshopDiag 处理 21/2，返回 503 物品诊断：
// 物品 GUID 和 152 组件列表，组件包含槽位、GUID 与折旧百分比。
func hWorkshopDiag(ctx *Ctx) ([]byte, bool) {
	srv := ctx.Sess.Srv
	if !srv.requireDB(ctx) {
		return nil, false
	}
	cctx := ctx.Sess.pushCtx()
	guid := ""
	if gs := deepGUIDs(ctx.Tree); len(gs) > 0 {
		guid = gs[0]
	}
	if guid == "" {
		guid = "00000000000000000000000000000000"
	}
	comps := proto.Args(proto.S("143")) // 元素类 hlopebmchhp(poolccanfhd, type_id 504) 的 args code
	if row, rerr := srv.DB.GetItemByGUID(cctx, guid); rerr == nil && row != nil {
		for _, p := range srv.repairPartsOf(cctx, row) {
			// f3 = 该零件的修理报价（按磨损计价，完好度 1.0 → 0）
			repairSilver := int64(math.Round(math.Max(0, 1-p.Cond) * 500))
			comps.Items = append(comps.Items, wiregen.MustMakeObj("hlopebmchhp", map[int]*proto.Value{
				0: proto.Guid(p.GUID),
				1: proto.U32(proto.F32Bits(float32(p.Cond))),
				2: proto.U32(4), // 官方固定 4
				3: priceObj(repairSilver, 0),
				4: proto.U32(0),
				// f5 = 可更换件工单列表（hpopgbjohpk[]），客户端据此显示
				// "选择替代品（有 N 种可选的变体）"。
				5: proto.Args(proto.S("146"), srv.partAlternatives(cctx, row.ItemID, p)...),
			}))
		}
	}
	obj := wiregen.MustMakeObj("mgjeoebmghh", map[int]*proto.Value{
		0: proto.Guid(guid),
		1: comps,
	})
	body, err := proto.EncodeValue(obj, ctx.Sess.Table())
	if err != nil {
		return nil, false
	}
	return body, true
}

// partOrder 是 21/3 请求里的一个换件条目。
type partOrder struct {
	Cat   uint32
	SysID string
}

// hWorkshopOrder 处理 21/3：按客户端提交的条目更换零件、扣费并返回更新后的物品树。
//
// 官方请求结构（抓包确认）：
//
//	args(524) [ workshopID,
//	            nfmkdcbnkhp { itemGUID,
//	                          args(503)[ {BYTE 品类, STR 新件 systemID} ... ],
//	                          是否金币, 客户端报价 } ]
func hWorkshopOrder(ctx *Ctx) ([]byte, bool) {
	srv := ctx.Sess.Srv
	if !srv.requireDB(ctx) {
		return nil, false
	}
	cctx := ctx.Sess.pushCtx()
	tree := ctx.Tree
	if tree == nil || tree.Kind != proto.KindArgs || len(tree.Items) < 2 {
		return nil, false
	}
	body := tree.Items[1]
	if body == nil || body.Kind != proto.KindObj || body.FieldCount() < 2 {
		return nil, false
	}
	gf := body.Field(0)
	itemGUID := gf.GuidHex()
	if itemGUID == "" {
		return nil, false
	}
	var orders []partOrder
	if lf := body.Field(1); lf.Val != nil && lf.Val.Kind == proto.KindArgs {
		for _, it := range lf.Val.Items {
			if it == nil || it.Kind != proto.KindObj || it.FieldCount() < 2 {
				continue
			}
			cf := it.Field(0)
			sf := it.Field(1)
			if sf.Str == nil || *sf.Str == "" {
				continue
			}
			orders = append(orders, partOrder{Cat: cf.U32(), SysID: *sf.Str})
		}
	}
	if len(orders) == 0 {
		return nil, false
	}
	useGold := false
	if of := body.Field(2); of.Num != nil {
		useGold = of.U32() != 0
	}
	clientCost := int64(0)
	if cf := body.Field(3); cf.Num != nil {
		clientCost = int64(cf.U64())
	}

	row, err := srv.DB.GetItemByGUID(cctx, itemGUID)
	if err != nil || row == nil || row.UID != ctx.Sess.Userid {
		return nil, false
	}
	fields, ferr := FieldsFromJSON(row.FieldsJSON)
	if ferr != nil {
		return nil, false
	}

	silver, gold := int64(0), int64(0)
	changed := false
	for _, o := range orders {
		idx := -1
		for i := range fields {
			if fields[i].Op != "VAL" || fields[i].Val == nil || fields[i].Val.Kind != proto.KindObj {
				continue
			}
			if srv.childCategory(cctx, fields[i].Val) == int64(o.Cat) {
				idx = i
				break
			}
		}
		if idx < 0 {
			continue
		}
		// 同型号 = 修理（恢复耐久，按磨损计价）；不同型号 = 更换。
		if installed := srv.childSystemID(cctx, fields[idx].Val); installed != "" &&
			strings.EqualFold(installed, o.SysID) {
			ch := fields[idx].Val
			if didx := fieldIdxByCS(ch.Class, csDurability); didx >= 0 && didx < ch.FieldCount() {
				old := float64(math.Float32frombits(ch.Fields[didx].U32()))
				ch.Fields[didx].Num = ptrU64(uint64(math.Float32bits(1.0)))
				silver += int64(math.Round(math.Max(0, 1-old) * 500))
			}
			changed = true
			continue
		}
		def, derr := srv.DB.GetItemDefByItemID(cctx, o.SysID)
		if derr != nil || def == nil {
			continue
		}
		child, cerr := srv.buildPartInstance(cctx, row, o.SysID, fmt.Sprintf("part-%d", idx))
		if cerr != nil || child == nil {
			continue
		}
		fields[idx].Val = child
		ps, pg := srv.partPrice(cctx, o.SysID, def.Category)
		silver += ps
		gold += pg
		changed = true
	}
	if !changed {
		return nil, false
	}
	costSilver, costGold := silver, gold
	if clientCost > 0 {
		if useGold {
			costGold = clientCost
		} else {
			costSilver = clientCost
		}
	}
	p, perr := srv.DB.EnsureProfile(cctx, ctx.Sess.Userid)
	if perr != nil {
		return nil, false
	}
	if useGold {
		if p.Gold < costGold {
			ctx.Log.Info("工坊换件：金币不足", "balance", p.Gold, "cost", costGold)
			return nil, false
		}
		_ = srv.DB.UpdateBalance(cctx, ctx.Sess.Userid, p.Silver, p.Gold-costGold, p.Bank)
		_ = srv.DB.AddTxn(cctx, ctx.Sess.Userid, "workshop_part", "", itemGUID, 0, -costGold, "gold")
	} else {
		if p.Silver < costSilver {
			ctx.Log.Info("工坊换件：银币不足", "balance", p.Silver, "cost", costSilver)
			return nil, false
		}
		_ = srv.DB.UpdateBalance(cctx, ctx.Sess.Userid, p.Silver-costSilver, p.Gold, p.Bank)
		_ = srv.DB.AddTxn(cctx, ctx.Sess.Userid, "workshop_part", "", itemGUID, -costSilver, 0, "silver")
	}
	// 版本戳必须严格递增，否则客户端会忽略更新后的物品树，工作坊停在转圈状态。
	bumpCreatedTicks(fields, row.WireClass)
	fj, jerr := FieldsToJSON(fields)
	if jerr != nil {
		return nil, false
	}
	// Use the updated parts when normalizing the snapshot before persistence.
	snapshot := *row
	snapshot.FieldsJSON = fj
	if row.WireClass == "dgjinoebdop" && srv.canonicalizeReelStrength(cctx, &snapshot, fields) {
		fj, jerr = FieldsToJSON(fields)
		if jerr != nil {
			return nil, false
		}
	}
	if err := srv.DB.UpdateItemFields(cctx, row.GUID, fj, row.Qty, row.Durability, row.Workmanship, row.RigGUID); err != nil {
		return nil, false
	}
	row.FieldsJSON = fj
	if def, derr := srv.itemDefForRow(cctx, row); derr == nil && def != nil && def.Category == gearCatReel {
		if _, perr := srv.syncReelPerformance(cctx, row); perr != nil {
			ctx.Log.Warn("工坊换件：轮子物理参数保存失败", "guid", shortGUID(row.GUID), "err", perr)
		}
	}
	out, terr := srv.ItemToTree(cctx, row)
	if terr != nil {
		return nil, false
	}
	bodyOut, berr := proto.EncodeValue(out, ctx.Sess.Table())
	if berr != nil {
		return nil, false
	}
	// 官方顺序是先回 21/3，再用 4/22 刷新客户端物品缓存。
	ctx.AfterResponse(func() {
		ctx.Sess.Push(4, 22, proto.Args(proto.S("61"), out))
	})
	ctx.Log.Info("21/3 工坊换件完成", "item", row.ItemID, "guid", shortGUID(row.GUID),
		"parts", len(orders), "silver", costSilver, "gold", costGold)
	return bodyOut, true
}

// hWorkshopRepair 构造 21/3 银币修理或 21/4 金币修理处理器。
// 优先使用客户端报价；f3<=0 时按本地公式估算。成功后耐久恢复为 1.0，
// 并返回重建后的物品树。
func hWorkshopRepair(useGold bool) Handler {
	return func(ctx *Ctx) ([]byte, bool) {
		srv := ctx.Sess.Srv
		if !srv.requireDB(ctx) {
			return nil, false
		}
		cctx := ctx.Sess.pushCtx()
		storeID, guid, slots, clientCost := parseRepairRequest(ctx.Tree)
		if guid == "" {
			return nil, false // 交给兜底链
		}
		row, err := srv.DB.GetItemByGUID(cctx, guid)
		if err != nil || row == nil {
			ctx.Log.Info("修理：物品不存在", "guid", shortGUID(guid))
			return nil, false
		}
		// 已完好则不必修理（避免白扣费）：回原物品状态
		if row.Durability >= 1.0 {
			ctx.Log.Info("修理：物品耐久已满", "guid", shortGUID(guid))
			tree, terr := srv.ItemToTree(cctx, row)
			if terr != nil {
				return nil, false
			}
			body, berr := proto.EncodeValue(tree, ctx.Sess.Table())
			if berr != nil {
				return nil, false
			}
			return body, true
		}
		wear := 1.0 - row.Durability
		cost := clientCost
		mapMult := 1.0
		if cost <= 0 {
			mapMult = repairMultiplierForStore(storeID, srv.currentLevel(ctx.Sess.Userid))
			cost = repairCost(len(slots), useGold, wear, mapMult)
		}
		p, perr := srv.DB.EnsureProfile(cctx, ctx.Sess.Userid)
		if perr != nil {
			return nil, false
		}
		balance := p.Silver
		if useGold {
			balance = p.Gold
		}
		if balance < cost {
			ctx.Log.Info("修理余额不足", "gold", useGold, "balance", balance, "cost", cost)
			if useGold {
				return nil, false // 金币不足时交给兜底链
			}
			// 21/3：回原物品状态（若可查）
			tree, terr := srv.ItemToTree(cctx, row)
			if terr != nil {
				return nil, false
			}
			body, berr := proto.EncodeValue(tree, ctx.Sess.Table())
			if berr != nil {
				return nil, false
			}
			return body, true
		}
		// 扣费 + 流水
		if useGold {
			_ = srv.DB.UpdateBalance(cctx, ctx.Sess.Userid, p.Silver, p.Gold-cost, p.Bank)
			_ = srv.DB.AddTxn(cctx, ctx.Sess.Userid, "repair", "", guid, 0, -cost, "gold")
		} else {
			_ = srv.DB.UpdateBalance(cctx, ctx.Sess.Userid, p.Silver-cost, p.Gold, p.Bank)
			_ = srv.DB.AddTxn(cctx, ctx.Sess.Userid, "repair", "", guid, -cost, 0, "silver")
		}
		// 恢复耐久，保留工艺质量。
		beforeDurability := row.Durability
		if err := srv.DB.SetItemDurability(cctx, guid, 1.0); err != nil {
			ctx.Log.Warn("修理耐久写库失败", "guid", shortGUID(guid), "err", err)
			return nil, false
		}
		row.Durability = 1.0
		if err := srv.DB.AddItemDamageLog(cctx, store.ItemDamageLog{
			UID: ctx.Sess.Userid, ItemGUID: guid, ItemID: row.ItemID, Event: damageEventRepair,
			BeforeDurability: ptrFloat64(beforeDurability), AfterDurability: ptrFloat64(1.0),
			DetailJSON: damageDetailJSON(map[string]any{"store": storeID, "cost": cost, "gold": useGold}),
		}); err != nil {
			ctx.Log.Warn("修理历史写库失败", "guid", shortGUID(guid), "err", err)
		}
		tree, terr := srv.ItemToTree(cctx, row)
		if terr != nil {
			return nil, false
		}
		body, berr := proto.EncodeValue(tree, ctx.Sess.Table())
		if berr != nil {
			return nil, false
		}
		ctx.Log.Info("修理完成", "guid", shortGUID(guid), "槽位数", len(slots),
			"费用", cost, "金", useGold, "磨损前", 1.0-wear,
			"店铺", storeID, "倍率", mapMult)
		return body, true
	}
}

// parseRepairRequest 解析 21/3、21/4 请求。
// 标准形状为 [str 物品GUID, obj(17510){GUID16, VAL 槽位, OPT 金币, U64 价格}]，
// 同时接受 [store, Guid 物品, 槽位...] 扁平形状。storeID 只用于估算修理费。
func parseRepairRequest(tree *proto.Value) (storeID, guid string, slots []int64, price int64) {
	strs := treeStrs(tree)
	if len(strs) > 0 {
		if isHex32(strs[0]) {
			guid = strs[0]
		} else {
			storeID = strs[0]
		}
	}
	if req := findObj(tree, "nfmkdcbnkhp"); req != nil && req.FieldCount() >= 4 {
		// f0 GUID16 物品 guid（非零覆盖）
		if g := req.Field(0).Hex; g != "" && g != "00000000000000000000000000000000" {
			guid = g
		}
		// f3 U64 客户端算好的修理费用
		if f3 := req.Field(3); f3.Num != nil {
			price = int64(f3.U64())
		}
		// f1 VAL 槽位数组（递归收集整数）
		if f1 := req.Field(1); f1.Val != nil {
			slots = deepInts(f1.Val)
		}
		if guid == "" {
			if gs := deepGUIDs(tree); len(gs) > 0 {
				guid = gs[0]
			}
		}
		return
	}
	// 扁平形状兜底（GM 工具和测试使用）
	if len(guid) == 0 {
		if gs := deepGUIDs(tree); len(gs) > 0 {
			guid = gs[0]
		}
	}
	if len(slots) == 0 {
		slots = deepInts(tree)
	}
	return
}

// deepInts 递归收集子树中的 u32/u64 整数。
func deepInts(v *proto.Value) []int64 {
	var out []int64
	var walk func(x *proto.Value)
	walk = func(x *proto.Value) {
		if x == nil {
			return
		}
		switch x.Kind {
		case proto.KindU32, proto.KindU64:
			out = append(out, int64(x.Num))
		case proto.KindArgs, proto.KindValList, proto.KindArray, proto.KindU32Arr, proto.KindGuidArr:
			for _, it := range x.Items {
				walk(it)
			}
		case proto.KindObj:
			for i := range x.Fields {
				if x.Fields[i].Num != nil {
					out = append(out, int64(u64Of(x.Fields[i].Num)))
				}
				if x.Fields[i].Val != nil {
					walk(x.Fields[i].Val)
				}
			}
		}
	}
	walk(v)
	return out
}

// ---------------------------------------------------------------------------
// 耐久衰减与修复
// ---------------------------------------------------------------------------

// wearPerCatch 定义每次成功钓获对各组件造成的耐久下降量。
var wearPerCatch = map[string]float64{
	"rod":     0.0002, // 竿本体：结构件，磨损慢于线/钩
	"line":    0.0015, // 鱼线：承力件，磨损最快
	"hook":    0.0012, // 鱼钩
	"bait":    0.0,    // 鱼饵是消耗品，由数量扣减而非耐久
	"float":   0.0004, // 浮漂
	"lure":    0.0008, // 路亚（反复抛投磨损）
	"default": 0.0006, // 其它组件
}

// slotWearKey 将组件槽位映射到磨损类别。
var slotWearKey = map[int64]string{
	1: "line", 2: "line", 3: "hook", 4: "bait", 5: "float",
	6: "lure", 7: "lure", 8: "lure",
}

// wearRigForCatch 在 14/4 成功后降低钓组各组件的耐久。
// 4/5 重建以 item.durability 列为准，更新该列即可同步字段。
func (s *Server) wearRigForCatch(ctx context.Context, rigGUID string) {
	if rigGUID == "" || s.DB == nil {
		return
	}
	comps, err := s.DB.ListRigComponents(ctx, rigGUID)
	if err != nil {
		return
	}
	for _, c := range comps {
		if c.GUID == "" {
			continue
		}
		key, ok := slotWearKey[c.Slot]
		if !ok {
			key = "default"
		}
		amount := wearPerCatch[key]
		if amount <= 0 {
			continue
		}
		row, err := s.DB.GetItemByGUID(ctx, c.GUID)
		if err != nil || row == nil {
			continue
		}
		newD := math.Max(0.0, row.Durability-amount)
		if math.Abs(newD-row.Durability) < 1e-9 {
			continue
		}
		if err := s.DB.SetItemDurability(ctx, row.GUID, newD); err == nil {
			s.log.Info("钓获磨损", "guid", shortGUID(row.GUID),
				"类别", key, "耐久", fmt.Sprintf("%.4f->%.4f", row.Durability, newD))
		}
	}
}

// ---------------------------------------------------------------------------
// 17/12 设钓深（handlers/fishing/17_12.py + item_factory.rod_rig_set_depth）
// ---------------------------------------------------------------------------

// hRodSetDepth 17/12（fire-and-forget，不回包）：竿实例 f5 钓棚深度（cm）落库
// + 会话状态记忆（14/1 的 158.f2 深度透传用）。
func hRodSetDepth(ctx *Ctx) ([]byte, bool) {
	srv := ctx.Sess.Srv
	rodGUID, depthCM := parseSetDepth(ctx.Tree)
	if rodGUID == "" || depthCM <= 0 {
		return nil, true
	}
	var row *store.Item
	if srv.DB != nil {
		row = srv.ownedRodRow(ctx.Sess.pushCtx(), ctx.Sess.Userid, rodGUID)
		if row == nil {
			ctx.Log.Warn("17/12 竿不属于当前玩家，已忽略", "竿", shortGUID(rodGUID))
			return nil, true
		}
	}
	st := srv.fishingStateForRequest(ctx)
	st.mu.Lock()
	st.DepthCM = depthCM
	st.mu.Unlock()
	ctx.Log.Info("17/12 设置钓深", "竿", shortGUID(rodGUID), "深度cm", depthCM)
	if row != nil && !srv.rodRigSetDepthRow(ctx.Sess.pushCtx(), row, depthCM) {
		ctx.Log.Info("17/12 竿实例无钓组字段，仅记会话深度", "竿", shortGUID(rodGUID))
	}
	return nil, true
}

// hRodSetDrag 17/14（fire-and-forget）：渔轮摩擦档位上报。
//
// 报文形状与 17/13（轮管卡）相同：args(52) + 竿 guid + u32。客户端每次启动都回到默认值，
// 是因为服务端没有把设定值记下来回填；这里按竿保存并写回钓组桩的第 6 个字段。
func hRodSetDrag(ctx *Ctx) ([]byte, bool) {
	srv := ctx.Sess.Srv
	rodGUID, value := parseRodGuidInt(ctx.Tree)
	if rodGUID == "" {
		return nil, true
	}
	ctx.Log.Info("17/14 设置摩擦", "竿", shortGUID(rodGUID), "值", value)
	if srv.DB != nil && !srv.setOwnedRodSetting(ctx.Sess.pushCtx(), ctx.Sess.Userid,
		rodGUID, reelDragSettingKey, value, 6) {
		ctx.Log.Warn("17/14 竿不属于当前玩家，已忽略", "竿", shortGUID(rodGUID))
	}
	return nil, true
}

// hRodSetSpeed 17/15（fire-and-forget）：路亚族收线速度档上报。
//
// 2026-11-07 抓包确认：调“收线速度”发 17/15（f5.f7），调“摩擦”发 17/14（f5.f6）。
// 服务端此前没有 17/15 handler，速度值被直接丢弃，重新登录后回到默认。
func hRodSetSpeed(ctx *Ctx) ([]byte, bool) {
	srv := ctx.Sess.Srv
	rodGUID, value := parseRodGuidInt(ctx.Tree)
	if rodGUID == "" {
		return nil, true
	}
	ctx.Log.Info("17/15 设置收线速度", "竿", shortGUID(rodGUID), "值", value)
	if srv.DB != nil {
		if !srv.setOwnedRodSetting(ctx.Sess.pushCtx(), ctx.Sess.Userid,
			rodGUID, reelSpeedSettingKey, value, 7) {
			ctx.Log.Warn("17/15 竿不属于当前玩家，已忽略", "竿", shortGUID(rodGUID))
			return nil, true
		}
	}
	st := srv.fishingStateForRequest(ctx)
	st.mu.Lock()
	st.ReelSetting = value
	st.mu.Unlock()
	return nil, true
}

// ownedRodRow 返回属于 uid 的竿实例行；竿不存在或归属不符时返回 nil。
func (s *Server) ownedRodRow(ctx context.Context, uid int64, rodGUID string) *store.Item {
	if s == nil || s.DB == nil {
		return nil
	}
	row, err := s.DB.GetItemByGUID(ctx, rodGUID)
	if err != nil || row == nil || row.UID != uid {
		return nil
	}
	return row
}

// setOwnedRodSetting 校验竿实例属于 uid 后，持久化竿设置并尽力回写 f5 的第 fieldIdx 个字段。
// 返回 false 表示竿不存在或不属于该玩家，此时不产生任何写入。
func (s *Server) setOwnedRodSetting(ctx context.Context, uid int64, rodGUID, key string, value int64, fieldIdx int) bool {
	row := s.ownedRodRow(ctx, uid, rodGUID)
	if row == nil {
		return false
	}
	_ = s.DB.UpsertRodSetting(ctx, rodGUID, key, strconv.FormatInt(value, 10))
	s.rodRigSetField(ctx, row, fieldIdx, value)
	return true
}

// rodRigSetField 把值写回竿实例钓组桩的第 fieldIdx 个字段，保留其他钓组状态。
func (s *Server) rodRigSetField(ctx context.Context, row *store.Item, fieldIdx int, value int64) bool {
	fields, err := FieldsFromJSON(row.FieldsJSON)
	if err != nil {
		return false
	}
	idx := fieldIdxByCS(row.WireClass, csRigObject)
	if idx < 0 || idx >= len(fields) {
		return false
	}
	stored := fields[idx].Val
	if stored == nil || stored.Kind != proto.KindObj || len(stored.Fields) <= fieldIdx {
		return false
	}
	clone := *stored
	clone.Fields = append([]proto.Field(nil), stored.Fields...)
	if clone.Fields[fieldIdx].Op != "VAL" {
		clone.Fields[fieldIdx].Op = "U32"
	}
	clone.Fields[fieldIdx].Num = ptrU64(uint64(value))
	fields[idx].Op = "VAL"
	fields[idx].Val = &clone
	s.saveItemFields(ctx, row, fields)
	return true
}

// hRodSetClip 17/13（fire-and-forget）：记录轮管卡米数，0 表示关闭。
func hRodSetClip(ctx *Ctx) ([]byte, bool) {
	srv := ctx.Sess.Srv
	rodGUID, clipM := parseRodGuidInt(ctx.Tree)
	if rodGUID == "" {
		return nil, true
	}
	clipM = clampReelClipM(clipM)
	ctx.Log.Info("17/13 设置轮管卡", "竿", shortGUID(rodGUID), "卡米", clipM)
	if srv.DB != nil {
		row := srv.ownedRodRow(ctx.Sess.pushCtx(), ctx.Sess.Userid, rodGUID)
		if row == nil {
			ctx.Log.Warn("17/13 竿不属于当前玩家，已忽略", "竿", shortGUID(rodGUID))
			return nil, true
		}
		_ = srv.DB.UpsertRodSetting(ctx.Sess.pushCtx(), rodGUID, reelClipSettingKey, strconv.FormatInt(clipM, 10))
		if !srv.rodRigSetClipRow(ctx.Sess.pushCtx(), row, clipM) {
			ctx.Log.Info("17/13 竿实例无钓组字段，仅记竿设置", "竿", shortGUID(rodGUID))
		}
	}
	return nil, true
}

// rodRigSetClipRow 更新已校验归属的竿实例 153.f5 轮管卡米数，保留其他钓组状态。
func (s *Server) rodRigSetClipRow(ctx context.Context, row *store.Item, clipM int64) bool {
	fields, err := FieldsFromJSON(row.FieldsJSON)
	if err != nil {
		return false
	}
	idx := fieldIdxByCS(row.WireClass, csRigObject)
	if idx < 0 || idx >= len(fields) {
		return false
	}
	stored := fields[idx].Val
	if stored == nil || stored.Kind != proto.KindObj {
		stored = nil
	}
	comps := rigCompsOf(stored)
	name, curType, ok := rigIdentity(stored)
	if !ok {
		curType = s.defaultRigTypeForRod(ctx, row.ItemID)
		if name == "" {
			name = rigNameForType(curType)
		}
	}
	depth := rigDepthOf(stored, defaultDepthFor(name, curType))
	fields[idx].Op = "VAL"
	fields[idx].Val = buildRig153Like(stored, name, curType, comps, depth, clipM)
	s.saveItemFields(ctx, row, fields)
	return true
}

// parseSetDepth 异构混合扫描（guid/u32 顺序敏感；17_12.py 的 raw_all 逃生舱）：
// 顶层第一个 guid = 竿，第一个 u32/i32 = 深度 cm。
func parseRodGuidInt(tree *proto.Value) (rodGUID string, value int64) {
	if tree == nil || tree.Kind != proto.KindArgs {
		return
	}
	gotInt := false
	for _, it := range tree.Items {
		if it == nil {
			continue
		}
		switch it.Kind {
		case proto.KindGuid:
			if it.Hex != "" && rodGUID == "" {
				rodGUID = it.Hex
			}
		case proto.KindU32, proto.KindU64:
			if !gotInt {
				value = int64(it.Num)
				gotInt = true
			}
		}
	}
	return
}

func parseSetDepth(tree *proto.Value) (rodGUID string, depthCM int64) {
	return parseRodGuidInt(tree)
}

// rodRigSetDepthRow 设置已校验归属的竿实例 f5 钓棚深度（厘米）并落库。
// 组件表和钓组类型保持不变；f5 缺失时先按竿型派生默认钓组。
func (s *Server) rodRigSetDepthRow(ctx context.Context, row *store.Item, depthCM int64) bool {
	fields, err := FieldsFromJSON(row.FieldsJSON)
	if err != nil {
		return false
	}
	idx := fieldIdxByCS(row.WireClass, csRigObject)
	if idx < 0 || idx >= len(fields) {
		return false // 不是竿类（无钓组字段）：不动它
	}
	stored := fields[idx].Val
	if stored == nil || stored.Kind != proto.KindObj {
		stored = nil
	}
	comps := rigCompsOf(stored)
	name, curType, typeOK := rigIdentity(stored)
	if !typeOK {
		curType = s.defaultRigTypeForRod(ctx, row.ItemID)
		if name == "" {
			name = rigNameForType(curType)
		}
	}
	fields[idx].Op = "VAL"
	fields[idx].Val = buildRig153Like(stored, name, curType, comps, depthCM, rigClipOf(stored))
	s.saveItemFields(ctx, row, fields)
	return true
}

// ---------------------------------------------------------------------------
// 蓝冠命名校验（systems/named_fish.py；14/6 的落库流程在 econ.go fishingRelease）
// ---------------------------------------------------------------------------

// superTrophyGrade 超级奖杯（蓝冠）= dohoaehalpj grade 4。
const superTrophyGrade = 4

// maxFishNameLen 名字长度上限（客户端输入框 + 服务端护栏，取保守值）。
const maxFishNameLen = 32

var (
	fishNameStripRe  = regexp.MustCompile(`[.\-_0-9\s]`)
	fishNameLatinRe  = regexp.MustCompile(`[a-zA-Z]`)
	fishNameCyrillic = regexp.MustCompile("[\u0400-\u04FF]")
)

// validateFishName 按客户端规则校验鱼名：
// 去掉 . -_ 空格 数字 后，剩余字符必须全拉丁或全西里尔。返回 (ok, 原因)。
func validateFishName(name string) (bool, string) {
	if name == "" {
		return false, "空名字"
	}
	s := strings.TrimSpace(name)
	if s == "" {
		return false, "空名字"
	}
	if utf8.RuneCountInString(s) > maxFishNameLen {
		return false, fmt.Sprintf("名字过长（>%d）", maxFishNameLen)
	}
	core := fishNameStripRe.ReplaceAllString(s, "")
	if core == "" {
		return false, "名字里没有字母"
	}
	hasLatin := fishNameLatinRe.MatchString(core)
	hasCyr := fishNameCyrillic.MatchString(core)
	if hasLatin && hasCyr {
		return false, "拉丁字母与西里尔字母不能混用"
	}
	if !hasLatin && !hasCyr {
		return false, "只能使用拉丁或西里尔字母（可带数字/空格/.-_）"
	}
	rest := fishNameLatinRe.ReplaceAllString(fishNameCyrillic.ReplaceAllString(core, ""), "")
	if rest != "" {
		return false, "含有不允许的字符"
	}
	return true, ""
}

// canNameFish 判断鱼是否可以命名：必须是蓝冠且尚未命名。
func canNameFish(ctx context.Context, db *store.DB, grade int64, fishID string) (bool, string) {
	if grade != superTrophyGrade {
		return false, fmt.Sprintf("只有蓝冠（超级奖杯）鱼可以命名（当前 grade=%d）", grade)
	}
	if fishID != "" && db.NamedFishIsNamed(ctx, fishID) {
		return false, "这条鱼已经被命名过，不能改名"
	}
	return true, ""
}

// releaseFishName 从 14/6 请求树取鱼名（args = [guid, guid, str]，取最后一个
// 非空 str；fishing._req_name）。
func releaseFishName(tree *proto.Value) string {
	names := treeStrsDeep(tree)
	for i := len(names) - 1; i >= 0; i-- {
		if names[i] != "" {
			return names[i]
		}
	}
	return ""
}

// insertNamedFish 蓝冠鱼命名落库（named_fish.name_fish）：校验名字 → 落
// named_fish 行（已命名冲突时 ON CONFLICT DO NOTHING = 保留原名）。
func (s *Server) insertNamedFish(ctx context.Context, uid int64, f *RolledFish, fishID, name, place string) (bool, string) {
	if fishID == "" {
		return false, "缺少鱼 id"
	}
	if ok, why := validateFishName(name); !ok {
		return false, why
	}
	clean := strings.TrimSpace(name)
	owner := ""
	if p, err := s.DB.EnsureProfile(ctx, uid); err == nil {
		owner = p.Name
	}
	err := s.DB.NamedFishInsert(ctx, &store.NamedFish{
		FishID:  fishID,
		Species: f.Species,
		Name:    clean,
		Owner:   owner,
		Place:   place,
		WeightG: f.WeightG,
		LengthM: f.LengthM,
		Grade:   f.Grade,
		TS:      float64(time.Now().Unix()),
	})
	if err != nil {
		return false, fmt.Sprintf("存储失败：%v", err)
	}
	return true, ""
}
