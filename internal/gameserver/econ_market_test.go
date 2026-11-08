package gameserver

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"testing"
	"time"

	"rf4server/internal/config"
	"rf4server/internal/logging"
	"rf4server/internal/proto"
	"rf4server/internal/store"
	"rf4server/internal/testutil"
	"rf4server/internal/wiregen"
)

// 钓鱼经济域测试，覆盖鱼市、结算时点和静态鱼库判鱼。

// importFishStaticForProbe 从仓库自带静态库导入钓鱼静态表（探针用）。
// 表清单与 seed 的 staticTables 相同；静态库缺失时跳过。
func importFishStaticForProbe(t *testing.T, db *store.DB, tables ...string) {
	t.Helper()
	if len(tables) == 0 {
		tables = []string{"loc_alias", "loc_wiki_fish", "loc_fish", "species_weight"}
	}
	if _, err := db.SQL.Exec(`ATTACH DATABASE ? AS src`, "../../server_data/static.sqlite3"); err != nil {
		t.Skip("静态库不可用: ", err)
	}
	defer func() { _, _ = db.SQL.Exec(`DETACH DATABASE src`) }()
	for _, table := range tables {
		if _, err := db.SQL.Exec(`INSERT INTO main.` + table + ` SELECT * FROM src.` + table); err != nil {
			t.Skipf("%s 导入失败: %v", table, err)
		}
	}
}

// TestRollFishUsesStaticTables 验证静态鱼库优先，鱼种来自地点名册并覆盖内置池；
// 无数据地点仍回退内置池。
func TestRollFishUsesStaticTables(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "roll.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	importFishStaticForProbe(t, db)
	srv := NewServer(config.Game{Tunnel: config.Tunnel{PSK: "roll"}}, logging.Discard(), nil)
	srv.SetDB(db)
	ctx := context.Background()

	// 静态名册（克马羚诺也湖 = level_001_lake 的统计站名）
	roster := map[string]bool{}
	rows, err := db.SQL.QueryContext(ctx,
		`SELECT fish_key FROM loc_wiki_fish WHERE loc=?`, "克马羚诺也湖")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		roster[k] = true
	}
	rows.Close()
	if len(roster) == 0 {
		t.Fatal("静态名册为空（导入失败？）")
	}

	// 静态路径命中，未回退。
	if f := srv.rollFishFromDB("loc1_02", "worm_01"); f == nil {
		t.Fatal("rollFishFromDB 应命中静态鱼库，不应回退到内置池")
	}

	// 鱼种应来自名册，并覆盖内置池之外的鱼种。
	hardPool := map[string]bool{
		"crucian": true, "c.roach": true, "perch": true,
		"freshwater_mussel": true, "ruffe": true,
	}
	seen := map[string]int{}
	const N = 300
	for i := 0; i < N; i++ {
		f := srv.rollFish("loc1_02", "worm_01")
		if f == nil {
			t.Fatal("rollFish 不应返回 nil")
		}
		if f.WeightG <= 0 {
			t.Errorf("克重应 >0: %+v", f)
		}
		if !roster[f.Species] {
			t.Errorf("鱼种 %q 不在静态名册", f.Species)
		}
		seen[f.Species]++
	}
	outside := 0
	for k := range seen {
		if !hardPool[k] {
			outside++
		}
	}
	if outside == 0 {
		t.Fatalf("%d 次 roll 全部落在内置鱼种池内，静态表未被使用：%v", N, seen)
	}
	t.Logf("静态判鱼 OK：%d 次 roll 覆盖 %d 鱼种（池外 %d 种）", N, len(seen), outside)

	// 无数据地点回退内置池。
	f := srv.rollFish("loc_no_such_place", "")
	if f == nil || !hardPool[f.Species] {
		t.Errorf("无数据地点应回退内置池，实得 %+v", f)
	}
}

// TestFishMarketFlow 验证钓获、14/4 结算和 20/2 卖鱼的完整流程：
// 结算只发经验；卖鱼按 20/1 报价入账一次，并删除护网行、发送 2/1 和 4/21 推送。
func TestFishMarketFlow(t *testing.T) {
	db := connectTestDB(t)
	defer db.Close()
	// 缩短咬钩等待（tunables 优先，tunableF 首读进缓存——必须在第一杆前写入）
	if err := db.TunableSet(context.Background(), "bite_delay_fixed", "0.3", "test"); err != nil {
		t.Fatal(err)
	}
	importFishStaticForProbe(t, db)
	gsAddr, srv := startGameserverWithDB(t, db)
	gw := testutil.StartGateway(t, gsAddr, testutil.TestPSK)

	token, uid := gw.Login(t)
	// 清残留
	{
		ctx0, cancel0 := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = db.SQL.ExecContext(ctx0, `DELETE FROM keepnet_items WHERE uid=?`, uid)
		_, _ = db.SQL.ExecContext(ctx0, `DELETE FROM catch_log WHERE uid=? AND source IN ('catch','keep')`, uid)
		_, _ = db.SQL.ExecContext(ctx0, `DELETE FROM txn WHERE uid=? AND kind IN ('sell_fish','sell_offer')`, uid)
		cancel0()
	}
	fc := testutil.NewFakeClient(t, gw.RealAddr)
	fc.Dial(token, false)
	if boot := fc.WaitPush(2 * time.Second); boot == nil || boot.Main != 0 {
		t.Fatalf("boot 0/1 缺失: %+v", boot)
	}
	cctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	p0, err := db.EnsureProfile(cctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	silver0 := p0.Silver
	table := wiregen.Table()

	// 取真实竿 guid
	rodGUID := ""
	{
		guids, _ := db.ItemGUIDsByContainers(context.Background(), uid, []string{"backpack", "rig"})
		for _, g := range guids {
			if row, err := db.GetItemByGUID(context.Background(), g); err == nil && row.WireClass == "cnolibojkao" {
				rodGUID = g
				break
			}
		}
	}
	if rodGUID == "" {
		t.Fatal("背包里没有竿（starter 播种失败？）")
	}

	bait, err := srv.CreateItem(cctx, uid, "worm_01", "backpack", 5)
	if err != nil {
		t.Fatal(err)
	}
	if result := srv.rigSetComponent(cctx, uid, rodGUID, catBait, bait.GUID, 350, rigSetOpts{ParentType: familyRod, EnforceCategory: true, SplitStack: true}); result.Rejected || result.MountedGUID == "" {
		t.Fatalf("failed to mount bait: %+v", result)
	}

	// 14/1 → 14/2
	fc.SendRequest(201, 14, 1, bodyWithGUIDs(rodGUID)...)
	if resp := fc.WaitResponse(3 * time.Second); resp == nil || resp.Body[0] != 0x01 {
		t.Fatalf("14/1 应回 158: %+v", resp)
	}
	fc.SendRequest(202, 14, 2, bodyWithGUIDs(rodGUID)...)
	if resp := fc.WaitResponse(3 * time.Second); resp == nil || resp.Body[0] != 0x01 {
		t.Fatalf("14/2 应回 800: %+v", resp)
	}
	backdateCastForFastBite(t, srv, uid)
	waterBody, err := proto.EncodeValue(proto.Args524(wiregen.MustMakeObj("lobemkloehp", map[int]*proto.Value{5: proto.U32(0x600)})), wiregen.Table())
	if err != nil {
		t.Fatal(err)
	}
	fc.SendRequest(203, 14, 7, waterBody...)
	// 测试使用固定短延迟并跳过落水保护。
	fish14 := waitPushCmd(t, fc, 14, 14, 3*time.Second)
	if fish14 == nil {
		t.Fatal("未收到 14/14")
	}
	dec, _, err := proto.DecodeValue(fish14.Body, 0, table)
	if err != nil || dec.ArgCount() != 2 {
		t.Fatalf("14/14 body 应为 args[2]: %v", err)
	}
	fishObj := dec.ArgItem(1)
	if fishObj == nil || fishObj.Class != "fgogccpglhb" {
		t.Fatalf("14/14 args[1] 应为 27: %+v", fishObj)
	}
	fishGUID := fishObj.Field(0).Hex
	if fishGUID == "" {
		t.Fatal("27.f0 鱼 guid 缺失")
	}
	fc.SendRequest(203, 14, 11) // 刺中（fire-and-forget，无响应；推 4/22）

	// 14/4 结算返回 801，但余额不变。
	fc.SendRequest(204, 14, 4)
	if resp := fc.WaitResponse(3 * time.Second); resp == nil {
		t.Fatal("14/4 无响应")
	} else if d, _, err := proto.DecodeValue(resp.Body, 0, table); err != nil || d.Class != "caagocpofin" {
		t.Fatalf("14/4 应回 801: %v %s", err, d.Class)
	}
	if p1, _ := db.EnsureProfile(cctx, uid); p1.Silver != silver0 {
		t.Fatalf("14/4 结算改变了银币 %d -> %d（应保持不变）", silver0, p1.Silver)
	}

	// 14/5 入护
	fc.SendRequest(205, 14, 5, bodyWithGUIDs(fishGUID)...)
	if resp := fc.WaitResponse(3 * time.Second); resp == nil || resp.Body[0] != 0x01 {
		t.Fatalf("14/5 应回 800: %+v", resp)
	}
	var nKeep int64
	if err := db.SQL.QueryRowContext(cctx,
		`SELECT COUNT(*) FROM keepnet_items WHERE uid=? AND echo_guid=?`, uid, fishGUID).Scan(&nKeep); err != nil || nKeep != 1 {
		t.Fatalf("入护后护网应恰 1 行: n=%d err=%v", nKeep, err)
	}

	// 20/1 询价：302 报价 > 0
	fc.SendRequest(206, 20, 1, bodyWithGUIDs(fishGUID)...)
	resp := fc.WaitResponse(3 * time.Second)
	dec, _, err = proto.DecodeValue(resp.Body, 0, table)
	if err != nil || dec.ArgCodeStr() != "111" || dec.ArgCount() != 1 {
		t.Fatalf("20/1 应回 args(111)[1 条]: %v code=%q n=%d", err, dec.ArgCodeStr(), dec.ArgCount())
	}
	q302 := dec.ArgItem(0)
	if q302 == nil || q302.Class != "jpmlmnfkbfn" || q302.Field(0).Hex != fishGUID {
		t.Fatalf("20/1 条目应为 302 且 guid 匹配: %+v", q302)
	}
	f1 := q302.Field(1)
	quote := int64(f1.U64())
	f2 := q302.Field(2)
	flag := int64(f2.U32())
	if quote <= 0 {
		t.Fatalf("20/1 报价应 >0: %d", quote)
	}
	if flag != 0 {
		t.Fatalf("刚入护的鱼不应腐败: flag=%d", flag)
	}

	// 咖啡厅订单独立于玩家护网。
	fc.SendRequest(207, 20, 3)
	resp = fc.WaitResponse(3 * time.Second)
	dec, _, _ = proto.DecodeValue(resp.Body, 0, table)
	if dec == nil || dec.ArgCodeStr() != "110" || dec.ArgCount() != 10 {
		t.Fatalf("20/3 应有10张独立订单: code=%q n=%d", dec.ArgCodeStr(), dec.ArgCount())
	}
	cafeID := dec.ArgItem(0).Field(0).Hex

	// 20/2 卖出：入账 = 报价（恰一次）
	fc.SendRequest(208, 20, 2, bodyWithGUIDs(fishGUID)...)
	resp = fc.WaitResponse(3 * time.Second)
	dec, _, err = proto.DecodeValue(resp.Body, 0, table)
	if err != nil || dec.ArgCodeStr() != "112" || dec.ArgCount() != 1 {
		t.Fatalf("20/2 应回 args(112)[1 条]: %v code=%q n=%d", err, dec.ArgCodeStr(), dec.ArgCount())
	}
	r303 := dec.ArgItem(0)
	if r303 == nil || r303.Class != "iajmdfbjjel" || r303.Field(0).Hex != fishGUID {
		t.Fatalf("20/2 明细应为 303 且 guid 匹配: %+v", r303)
	}
	rf1 := r303.Field(1)
	paid := int64(rf1.U64())
	if paid != quote {
		t.Fatalf("20/2 入账 %d ≠ 20/1 报价 %d", paid, quote)
	}
	p2, _ := db.EnsureProfile(cctx, uid)
	if p2.Silver != silver0+quote {
		t.Fatalf("银币应恰一次入账 +%d（%d -> %d），实得 %d", quote, silver0, silver0+quote, p2.Silver)
	}
	var txnN, txnSum int64
	if err := db.SQL.QueryRowContext(cctx,
		`SELECT COUNT(*), COALESCE(SUM(silver_delta),0) FROM txn WHERE uid=? AND kind='sell_fish'`,
		uid).Scan(&txnN, &txnSum); err != nil || txnN != 1 || txnSum != quote {
		t.Fatalf("sell_fish 流水应恰 1 条且金额=报价: n=%d sum=%d quote=%d err=%v", txnN, txnSum, quote, err)
	}
	if err := db.SQL.QueryRowContext(cctx,
		`SELECT COUNT(*) FROM keepnet_items WHERE uid=? AND echo_guid=?`, uid, fishGUID).Scan(&nKeep); err != nil || nKeep != 0 {
		t.Fatalf("卖出后护网行应删除: n=%d err=%v", nKeep, err)
	}
	// 推送：2/1（银币）与 4/21（移除）
	if waitPushCmd(t, fc, 4, 21, 3*time.Second) == nil {
		t.Fatal("20/2 后未收到 4/21 移除推送")
	}

	// 卖鱼不清空咖啡厅需求。
	fc.SendRequest(209, 20, 3)
	resp = fc.WaitResponse(3 * time.Second)
	dec, _, _ = proto.DecodeValue(resp.Body, 0, table)
	if dec == nil || dec.ArgCodeStr() != "110" || dec.ArgCount() != 10 || dec.ArgItem(0).Field(0).Hex != cafeID {
		t.Fatalf("20/3 卖后应保留原订单: code=%q n=%d", dec.ArgCodeStr(), dec.ArgCount())
	}

	// 21/1 工坊列表：无磨损物品时空 args(144)
	fc.SendRequest(210, 21, 1)
	resp = fc.WaitResponse(3 * time.Second)
	dec, _, err = proto.DecodeValue(resp.Body, 0, table)
	if err != nil || dec.Kind != proto.KindArgs || dec.ArgCodeStr() != "144" {
		t.Fatalf("21/1 应回 args(144): %v code=%q", err, dec.ArgCodeStr())
	}
}

// TestMarketPricingModel 验证新鲜度、腐败和鱼市计价。
func TestMarketPricingModel(t *testing.T) {
	now := float64(time.Now().Unix())
	// 新鲜度阶梯（FRESHNESS_TIERS）
	cases := []struct {
		ageH float64
		want float64
	}{
		{0, 1.0}, {23, 1.0}, {24, 1.0}, {25, 0.8}, {48, 0.8}, {49, 0.6}, {72, 0.6}, {73, 0.4}, {1000, 0.4},
	}
	for _, c := range cases {
		if got := freshnessFactor(now-c.ageH*3600, now); got != c.want {
			t.Errorf("freshness(%vh)=%v want %v", c.ageH, got, c.want)
		}
	}
	// 腐败判据：> 86400s（不是 >=）
	if isFishRotten(now-86400, now) {
		t.Error("恰 86400s 不应判腐败")
	}
	if !isFishRotten(now-86401, now) {
		t.Error("86401s 应判腐败")
	}
	// 报价：perch g2 782g 鲜鱼 = round(0.1176/28 × 782 × 100 × 每日系数)
	e := &store.KeepnetEntry{Species: "perch", Cat: 2, WeightG: 782, TS: now}
	wantPerch := int64(math.Max(1, math.Round(0.1176/28.0*782*100*dailyMarketFactor("perch", now))))
	if got := marketPriceCents(e, now); got != wantPerch {
		t.Errorf("perch g2 782g 报价 = %d want %d", got, wantPerch)
	}
	// 未标定种兜底（MARKET_RATE_FALLBACK g2=0.10/28）
	e2 := &store.KeepnetEntry{Species: "no_such_fish", Cat: 2, WeightG: 100, TS: now}
	wantFallback := int64(math.Max(1, math.Round(0.10/28.0*100*100*dailyMarketFactor("no_such_fish", now))))
	if got := marketPriceCents(e2, now); got != wantFallback {
		t.Errorf("未标定种兜底报价 = %d", got)
	}
	// g1 = g2 价 ×0.45
	if got := marketRatePerG("c.roach", 1); got != mathRound6(0.0990*g1MarketRatio/28.0) {
		t.Errorf("c.roach g1 速率 = %v", got)
	}
}

// TestWorkshopRepairCost 验证修理费用公式。
func TestWorkshopRepairCost(t *testing.T) {
	if got := repairCost(0, false, 1.0, 1.0); got != 300 {
		t.Errorf("银修（1 槽全额磨损）= %d want 300", got)
	}
	if got := repairCost(3, false, 0.5, 1.0); got != 250 {
		t.Errorf("银修（3 槽半磨损）= %d want 250", got)
	}
	if got := repairCost(0, false, 0.0, 1.0); got != 75 { // 下限 25%
		t.Errorf("银修（磨损下限）= %d want 75", got)
	}
	if got := repairCost(0, true, 1.0, 1.0); got != 1 {
		t.Errorf("金修 = %d want 1", got)
	}
}

// TestWorkshopRepairMapAndBrandMultiplier 验证地图倍率、品牌覆盖、
// 店铺地图优先级和未知数据回退。
func TestWorkshopRepairMapAndBrandMultiplier(t *testing.T) {
	cases := []struct {
		name    string
		mapKey  string
		brands  []string
		storeID string
		want    float64
	}{
		{name: "level_001", mapKey: "level_001_lake", want: 1.0},
		{name: "惟有诺克河", mapKey: "level_002_kirzah_river", want: 0.85},
		{name: "level_003", mapKey: "level_003_viiveri_lake", want: 2.0},
		{name: "拉多加通用", mapKey: "level_006_ladoga", want: 1.35},
		{name: "拉多加Beluga", mapKey: "level_006_ladoga", brands: []string{"beluga"}, want: 0.85},
		{name: "琥珀湖Zeiman", mapKey: "level_014_carp_lake", brands: []string{"zeiman"}, want: 0.85},
		{name: "未知图", mapKey: "no_such_map", want: 1.0},
		{name: "店铺地图优先", storeID: "workshop_02", mapKey: "level_001_lake", want: 0.85},
		{name: "店铺品牌覆盖", storeID: "belugaworkshop_006", mapKey: "level_002_kirzah_river", want: 0.85},
		{name: "未知店铺走当前图", storeID: "unknown_store", mapKey: "level_002_kirzah_river", want: 0.85},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := repairMultForMap(tc.mapKey, tc.brands)
			if tc.storeID != "" {
				got = repairMultiplierForStore(tc.storeID, tc.mapKey)
			}
			if math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("mult=%v want %v", got, tc.want)
			}
		})
	}
	if got := repairCost(1, false, 0.5, repairMultiplierForStore("workshop_02", "level_001_lake")); got != 128 {
		t.Fatalf("惟有诺克河 1 槽半磨损修理费=%d want 128", got)
	}
}

// TestWorkshopRepairFallbackUsesStoreMapAndBrand 21/3 在客户端报价=0 时，
// fallback 必须走店铺所属地图/品牌倍率，而不是恒定 1.0。
func TestWorkshopRepairFallbackUsesStoreMapAndBrand(t *testing.T) {
	srv, db := newEconomyTxTestServer(t)
	ctx0 := context.Background()
	const uid = int64(3301)
	if err := srv.EnsurePlayer(ctx0, uid); err != nil {
		t.Fatal(err)
	}
	guids, err := db.ItemGUIDsByContainers(ctx0, uid, []string{"backpack", "rig"})
	if err != nil || len(guids) < 3 {
		t.Skipf("测试角色可修物品不足: n=%d err=%v", len(guids), err)
	}
	cases := []struct {
		store string
		want  int64
	}{
		{store: "workshop_viiveri", want: 300}, // level_003_viiveri_lake ×2.00
		{store: "workshop_02", want: 128},      // 惟有诺克河 ×0.85，300×0.5×0.85=127.5→128
		{store: "workshop_01", want: 150},      // level_001_lake ×1.00
	}
	for i, tc := range cases {
		guid := guids[i]
		if err := db.SetItemDurability(ctx0, guid, 0.5); err != nil {
			t.Fatal(err)
		}
		before, err := db.GetProfile(ctx0, uid)
		if err != nil {
			t.Fatal(err)
		}
		req := proto.Args524(
			proto.StrVal(tc.store),
			proto.Guid(guid),
			proto.U32Arr([]uint32{1}),
		)
		body, ok := hWorkshopRepair(false)(&Ctx{
			Sess: &Session{ID: 1, Userid: uid, Srv: srv, log: logging.Discard()},
			Main: 21, Sub: 3, Tree: req, DecodeOK: true, Log: logging.Discard(),
		})
		if !ok || len(body) == 0 {
			t.Fatalf("%s 21/3 fallback 未处理", tc.store)
		}
		after, err := db.GetProfile(ctx0, uid)
		if err != nil {
			t.Fatal(err)
		}
		if got := before.Silver - after.Silver; got != tc.want {
			t.Fatalf("%s fallback 扣费=%d want %d", tc.store, got, tc.want)
		}
	}
}

// TestFishNameValidation 验证蓝冠鱼命名规则。
func TestFishNameValidation(t *testing.T) {
	ok := []string{"Big Fish", "Karp-1", "Лещ_2", "a.b_c d9"}
	bad := map[string]string{
		"":         "空名字",
		"   ":      "空名字",
		"BigЛещ":   "混用",
		"12345":    "无字母",
		"Big*Fish": "非法字符",
	}
	for _, s := range ok {
		if o, why := validateFishName(s); !o {
			t.Errorf("%q 应通过: %s", s, why)
		}
	}
	for s := range bad {
		if o, _ := validateFishName(s); o {
			t.Errorf("%q 应拒绝", s)
		}
	}
	long := make([]byte, maxFishNameLen+1)
	for i := range long {
		long[i] = 'a'
	}
	if o, _ := validateFishName(string(long)); o {
		t.Error("超长名字应拒绝")
	}
}

// TestLevelKeyOfLoc 地点解析（fishing.level_of 口径）。
func TestLevelKeyOfLoc(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "loc.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	importFishStaticForProbe(t, db, "loc_alias")
	srv := NewServer(config.Game{Tunnel: config.Tunnel{PSK: "loc"}}, logging.Discard(), nil)
	srv.SetDB(db)
	ctx := context.Background()
	cases := map[string]string{
		"level_001_lake": "level_001_lake",
		"loc1_02":        "level_001_lake",
		"lvl2_01":        "level_002_kirzah_river",
		"loc9_01":        "level_009_ahtuba",
	}
	for loc, want := range cases {
		if got := srv.levelKeyOfLoc(ctx, 1, loc); got != want {
			t.Errorf("levelKeyOfLoc(%q)=%q want %q", loc, got, want)
		}
	}
}

// TestRepairFlow 银修理闭环：磨损 → 21/3 按客户端报价扣费 → 耐久回 1.0 →
// 4/5 重建树耐久字段同步（durability 生命周期最小模型）。
func TestRepairFlow(t *testing.T) {
	db := openItemTestDB(t)
	ctx := context.Background()
	srv := NewServer(config.Game{Tunnel: config.Tunnel{PSK: "repair"}}, logging.Discard(), nil)
	srv.SetDB(db)
	if err := srv.EnsurePlayer(ctx, 1000001); err != nil {
		t.Fatal(err)
	}
	sess := newProbeSess(srv)

	// 造一件磨损物品（可用背包任一竿类实例）
	guids, _ := db.ItemGUIDsByContainers(ctx, 1000001, []string{"backpack", "rig"})
	if len(guids) == 0 {
		t.Skip("无背包物品")
	}
	guid := guids[0]
	if err := db.SetItemDurability(ctx, guid, 0.5); err != nil {
		t.Fatal(err)
	}
	p0, _ := db.EnsureProfile(ctx, 1000001)
	silver0 := p0.Silver

	// 21/3 请求：args[str guid, obj(17510){GUID16, VAL, OPT, U64 价 120}]
	req := proto.Args524(
		proto.StrVal(guid),
		wiregen.MustMakeObj("nfmkdcbnkhp", map[int]*proto.Value{
			0: proto.Guid(guid),
			1: proto.TypedU32Arr("17", []uint32{11}),
			2: proto.U32(0),
			3: proto.U64(120),
		}),
	)
	body, err := proto.EncodeValue(req, sess.Table())
	if err != nil {
		t.Fatal(err)
	}
	b, ok := hWorkshopRepair(false)(&Ctx{Sess: sess, Main: 21, Sub: 3, Tree: req, DecodeOK: true, Body: body, Log: logging.Discard()})
	if !ok || len(b) == 0 {
		t.Fatal("21/3 未处理")
	}
	if _, _, err := proto.DecodeValue(b, 0, sess.Table()); err != nil {
		t.Fatalf("21/3 响应不可解码: %v", err)
	}
	// 耐久回满
	row, _ := db.GetItemByGUID(ctx, guid)
	if row == nil || row.Durability != 1.0 {
		t.Fatalf("修理后耐久应 1.0: %+v", row)
	}
	// 修理历史必须落库，且记录修复前后耐久。
	damageLogs, err := db.ListItemDamageLogs(ctx, 1000001, 20)
	if err != nil {
		t.Fatal(err)
	}
	foundRepairLog := false
	for _, entry := range damageLogs {
		if entry.ItemGUID == guid && entry.Event == damageEventRepair &&
			entry.BeforeDurability != nil && *entry.BeforeDurability == 0.5 &&
			entry.AfterDurability != nil && *entry.AfterDurability == 1.0 {
			foundRepairLog = true
			break
		}
	}
	if !foundRepairLog {
		t.Fatalf("缺少修理历史: %+v", damageLogs)
	}
	// 按客户端报价扣费
	p1, _ := db.EnsureProfile(ctx, 1000001)
	if p1.Silver != silver0-120 {
		t.Fatalf("应按客户端报价扣 120 银： %d -> %d", silver0, p1.Silver)
	}
	var txnN int64
	if err := db.SQL.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM txn WHERE uid=? AND kind=? AND silver_delta=?`,
		int64(1000001), "repair", -120).Scan(&txnN); err != nil {
		t.Fatalf("txn 查询失败: %v", err)
	}
	if txnN != 1 {
		t.Fatalf("repair 流水应 1 条: %d", txnN)
	}
	// 4/5 重建树耐久字段应为 1.0（列权威覆盖 nmidlgejlcp）
	tree, err := srv.ItemToTree(ctx, row)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := json.Marshal(skeletonOf(tree))
	t.Logf("修理后树骨架: %s", b2)
	// 再次修理（已满耐久）：不扣费
	if b, ok := hWorkshopRepair(false)(&Ctx{Sess: sess, Main: 21, Sub: 3, Tree: req, DecodeOK: true, Log: logging.Discard()}); !ok {
		t.Fatal("满耐久 21/3 应回物品树")
	} else if _, _, err := proto.DecodeValue(b, 0, sess.Table()); err != nil {
		t.Fatalf("满耐久 21/3 响应不可解码: %v", err)
	}
	p2, _ := db.EnsureProfile(ctx, 1000001)
	if p2.Silver != p1.Silver {
		t.Fatalf("满耐久不应扣费: %d -> %d", p1.Silver, p2.Silver)
	}
}

func mathRound(x float64) float64 { return float64(int64(x + 0.5)) }

func mathRound6(x float64) float64 { return float64(int64(x*1e6+0.5)) / 1e6 }
