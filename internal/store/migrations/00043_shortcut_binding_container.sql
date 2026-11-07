-- 快捷栏绑定只应随物品归属变化、或被放进不支持的容器（鱼护、导入的原生容器）而失效。
-- 旧触发器把 backpack/rig 之外的所有容器都当成"不可用"，
-- 于是竿子放进帐篷/车/船/滑板车/房子后绑定被删，客户端按更短列表重新编号，
-- 表现为快捷栏槽位错位（1 号竿变 2 号）。
DROP TRIGGER IF EXISTS clear_unavailable_shortcut_aliases;

CREATE TRIGGER clear_unavailable_shortcut_aliases AFTER UPDATE OF uid,container ON item
WHEN NEW.uid<>OLD.uid
  OR NEW.container NOT IN ('backpack','rig','tent','car','boat','scooter','house')
BEGIN
    DELETE FROM container_slot WHERE uid=OLD.uid AND guid=OLD.guid;
    UPDATE kv SET v=(SELECT COALESCE(json_group_object(key,value),'{}')
                     FROM json_each(kv.v) WHERE value<>OLD.guid)
    WHERE k='shortcut-aliases:' || OLD.uid AND json_valid(v);
END;
