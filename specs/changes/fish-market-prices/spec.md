# 鱼市价格表与每日波动

Status: ready

## Problem Statement

鱼市覆盖价只有 2 个地图、42 条记录，绝大多数鱼种走 `marketRatePerG` 基础速率兜底；客户端 20/1 询价携带的是 `level_004_fishmarket` 这类场景键，价格表查不到目标水域后同样回退基础速率，覆盖价实际等于没生效。鱼价全天固定，同一条鱼每天价格不变。

## Solution

鱼市按“基准价 × 水域倍率”定价：价格表只保存 `market_pct = 1.0` 的基准水域，其余水域由 `map_market_multipliers.json` 的倍率折算；客户端场景键归一到真实地图键；同一鱼种按 UTC 日期产生 ±5% 的日系数，同一天内稳定，跨天变化。

## User Stories

1. 作为玩家，同一鱼种在克马羚诺也湖（0.831）比阿赫图巴河（1.0）卖得便宜，符合各水域鱼市倍率。
2. 作为玩家，同一天内反复询价，报价保持一致；跨天后同一批鱼价格整体变化，单条浮动不超过 ±5%。
3. 作为运维，价格表缺失某鱼种时仍按基础速率兜底，不报错、不返回 0。

## Implementation Decisions

- `internal/gameserver/assets/fish_market_prices.json`：只保留基准水域 `level_009_ahtuba`（`market_pct = 1.0`）的 251 个鱼种四档价格，`source` 保留 rf4db URL。
- `internal/gameserver/assets/map_market_multipliers.json`：拉多加湖 `market_pct` 由 1.0 校正为 1.10，与鱼市倍率表一致。
- `marketSilverPerKgOverlay`：新增 `normalizeMarketMapKey`，把 `<map>_fishmarket` 归一到真实地图键；命中其他水域表时按 `v / basePct × targetPct` 折算。
- `dailyMarketFactor`：按 UTC 日期与鱼种做 FNV 哈希，取值 0.95–1.05；日期来源为包级变量，可用环境变量 `RF4_MARKET_DAY` 覆盖，计价路径不做文件 I/O。
- 不改表结构、不改协议字段；20/1 与 20/2 共用 `marketPriceCentsForMap`。

## Testing Decisions

- 单元层：`TestMarketOverlayAppliesMapMultiplier` 验证 0.831 / 0.916 / 1.0 三个水域价差与折算比例；`TestMarketOverlayNormalizesFishmarketKey` 验证场景键归一。
- 行为层：`TestDailyMarketFactorRangeAndStability` 验证系数范围、同日稳定、跨日变化；`TestMarketPriceStableWithinSameInstant` 验证同一时刻 20/1 与 20/2 同价。
- 期望值来自独立真值：倍率取自 `map_market_multipliers.json` 的登记值，系数范围取自产品约定 ±5%，不是把实现重算一遍。
- 回滚后 `TestMarketOverlayAppliesMapMultiplier` 会变红（同价时三档相等），`TestDailyMarketFactorRangeAndStability` 在去掉系数后同样变红。

## Out of Scope

- 稀有 / 超稀有鱼的倍数与蓝冠价维持现有截图口径，不在本次调整范围。
- 未按真实逐图成交价重建价格表，只保留基准价 + 倍率折算。
- 小欧鳊没有价格数据，继续走基础速率兜底。
