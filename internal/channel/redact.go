package channel

import (
	"encoding/json"
	"sort"
	"strings"
)

// 凭据不进错误文本：适配器自己保证错误里不拼 secret / token（约定），渠道层在写 last_error、jobs.last_error、
// 日志之前再过一遍 Redact（双保险）—— last_error 会回显到后台，jobs.last_error 谁都能在库里看到。

// redactMin 是要替换的最短值：太短的（"1"、"on"）替换了只会把错误文本弄乱，也不像凭据。
const redactMin = 6

// Redact 把 secrets（JSON 对象，可嵌套）里所有长度 ≥ 6 的字符串值在 msg 里替换成 "***"。
// secrets 为空或解不开时原样返回 msg。
func Redact(msg string, secrets json.RawMessage) string {
	if len(secrets) == 0 || msg == "" {
		return msg
	}
	var v any
	if json.Unmarshal(secrets, &v) != nil {
		return msg
	}
	var vals []string
	collectStrings(v, &vals)
	// 长的先换：一个值是另一个的子串时，先换短的会留下长值的残片。
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	for _, s := range vals {
		msg = strings.ReplaceAll(msg, s, "***")
	}
	return msg
}

func collectStrings(v any, out *[]string) {
	switch x := v.(type) {
	case string:
		if len(x) >= redactMin {
			*out = append(*out, x)
		}
	case map[string]any:
		for _, e := range x {
			collectStrings(e, out)
		}
	case []any:
		for _, e := range x {
			collectStrings(e, out)
		}
	}
}

// RedactError 返回一个 Error() 已脱敏、但 errors.Is / errors.As 仍能穿透到原错误的包装。err 为 nil 返回 nil；
// 脱敏前后文本相同时原样返回 err。
func RedactError(err error, secrets json.RawMessage) error {
	if err == nil {
		return nil
	}
	msg := Redact(err.Error(), secrets)
	if msg == err.Error() {
		return err
	}
	return &redactedError{msg: msg, err: err}
}

type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }
