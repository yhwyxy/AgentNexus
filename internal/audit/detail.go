package audit

import "encoding/json"

// Detail 是审计详情的最小构造器：把键值集合编码为 JSON 对象。
//
// 编码失败（例如值里含 NaN）退化为空对象：事件本身仍要落库，详情只是补充信息。
// 键名是否敏感交由 Record 的 deny-list 处理，发射点不需要各自维护一份过滤规则。
func Detail(fields map[string]any) json.RawMessage {
	encoded, err := json.Marshal(fields)
	if err != nil {
		return json.RawMessage(`{}`)
	}

	return encoded
}
