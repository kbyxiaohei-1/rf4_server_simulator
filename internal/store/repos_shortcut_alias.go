package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

func shortcutAliasKey(uid int64) string { return fmt.Sprintf("shortcut-aliases:%d", uid) }

func sortedShortcuts(bindings map[int64]string) []Shortcut {
	result := make([]Shortcut, 0, len(bindings))
	for slot, guid := range bindings {
		result = append(result, Shortcut{Slot: slot, GUID: guid})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Slot < result[j].Slot })
	return result
}

func shortcutItemAvailable(ctx context.Context, tx *sql.Tx, uid int64, guid string) (bool, error) {
	var count int
	err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM item WHERE uid=? AND guid=? AND container IN ('backpack','rig')", uid, guid).Scan(&count)
	return count == 1, err
}

func loadShortcutBindings(ctx context.Context, tx *sql.Tx, uid int64) (map[int64]string, error) {
	bindings := map[int64]string{}
	// 读取绑定时不限制容器：竿子会在 backpack/rig/船/车等容器之间移动，
	// 绑定不该因为换了容器就消失（否则客户端会把快捷栏重新编号，出现槽位错位）。
	rows, err := tx.QueryContext(ctx, `SELECT cs.slot,cs.guid FROM container_slot cs JOIN item i ON i.guid=cs.guid AND i.uid=cs.uid WHERE cs.uid=? AND cs.container=0`, uid)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var slot int64
		var guid string
		if err = rows.Scan(&slot, &guid); err != nil {
			_ = rows.Close()
			return nil, err
		}
		bindings[slot] = guid
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	var raw string
	err = tx.QueryRowContext(ctx, "SELECT v FROM kv WHERE k=?", shortcutAliasKey(uid)).Scan(&raw)
	if err == sql.ErrNoRows {
		return bindings, nil
	}
	if err != nil {
		return nil, err
	}
	var aliases map[string]string
	if err = json.Unmarshal([]byte(raw), &aliases); err != nil {
		return nil, fmt.Errorf("快捷栏别名无效: %w", err)
	}
	for key, guid := range aliases {
		slot, err := strconv.ParseInt(key, 10, 32)
		if err != nil || slot < 0 {
			return nil, fmt.Errorf("快捷栏槽位无效")
		}
		if _, ok := bindings[slot]; ok {
			continue
		}
		available, err := shortcutItemAvailable(ctx, tx, uid, guid)
		if err != nil {
			return nil, err
		}
		if available {
			bindings[slot] = guid
		}
	}
	return bindings, nil
}

// SaveShortcutsTx stores one physical position per item and preserves its extra hotkey aliases.
func SaveShortcutsTx(ctx context.Context, tx *sql.Tx, uid int64, bindings map[int64]string) error {
	for slot, guid := range bindings {
		bytes, err := hex.DecodeString(guid)
		if slot < 0 || slot > 2147483647 || err != nil || len(bytes) != 16 || guid == strings.Repeat("0", 32) {
			return fmt.Errorf("快捷栏绑定无效")
		}
		available, err := shortcutItemAvailable(ctx, tx, uid, guid)
		if err != nil {
			return err
		}
		if !available {
			return fmt.Errorf("物品不在当前角色背包中")
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM container_slot WHERE uid=? AND container=0", uid); err != nil {
		return err
	}
	seen := map[string]bool{}
	aliases := map[string]string{}
	for _, binding := range sortedShortcuts(bindings) {
		if seen[binding.GUID] {
			aliases[strconv.FormatInt(binding.Slot, 10)] = binding.GUID
			continue
		}
		seen[binding.GUID] = true
		if _, err := tx.ExecContext(ctx, "INSERT INTO container_slot(uid,container,slot,guid) VALUES(?,0,?,?)", uid, binding.Slot, binding.GUID); err != nil {
			return err
		}
	}
	if len(aliases) == 0 {
		_, err := tx.ExecContext(ctx, "DELETE FROM kv WHERE k=?", shortcutAliasKey(uid))
		return err
	}
	raw, err := json.Marshal(aliases)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO kv(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v", shortcutAliasKey(uid), string(raw))
	return err
}
