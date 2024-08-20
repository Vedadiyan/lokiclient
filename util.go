package lokiclient

import "github.com/vedadiyan/lokiclient/internal/json"

func Encode(v any) []byte {
	return json.Marshal(v)
}
