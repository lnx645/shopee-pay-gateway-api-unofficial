package main

import (
	"encoding/json"
	"strconv"
	"time"
)

func jsonNumber(i int64) json.Number { return json.Number(strconv.FormatInt(i, 10)) }
func secTime(unix int64) time.Time   { return time.Unix(unix, 0) }
