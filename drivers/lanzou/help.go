package lanzou

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const DAY time.Duration = 84600000000000

// 解析时间
var timeSplitReg = regexp.MustCompile("([0-9.]*)\\s*([\u4e00-\u9fa5]+)")

// 如果解析失败,则返回当前时间
func MustParseTime(str string) time.Time {
	lastOpTime, err := time.ParseInLocation("2006-01-02 -07", str+" +08", time.Local)
	if err != nil {
		strs := timeSplitReg.FindStringSubmatch(str)
		lastOpTime = time.Now()
		if len(strs) == 3 {
			i, _ := strconv.ParseInt(strs[1], 10, 64)
			ti := time.Duration(-i)
			switch strs[2] {
			case "秒前":
				lastOpTime = lastOpTime.Add(time.Second * ti)
			case "分钟前":
				lastOpTime = lastOpTime.Add(time.Minute * ti)
			case "小时前":
				lastOpTime = lastOpTime.Add(time.Hour * ti)
			case "天前":
				lastOpTime = lastOpTime.Add(DAY * ti)
			case "昨天":
				lastOpTime = lastOpTime.Add(-DAY)
			case "前天":
				lastOpTime = lastOpTime.Add(-DAY * 2)
			}
		}
	}
	return lastOpTime
}

// 解析大小
var sizeSplitReg = regexp.MustCompile(`(?i)([0-9.]+)\s*([bkm]+)`)

// 解析失败返回0
func SizeStrToInt64(size string) int64 {
	strs := sizeSplitReg.FindStringSubmatch(size)
	if len(strs) < 3 {
		return 0
	}

	s, _ := strconv.ParseFloat(strs[1], 64)
	switch strings.ToUpper(strs[2]) {
	case "B":
		return int64(s)
	case "K":
		return int64(s * (1 << 10))
	case "M":
		return int64(s * (1 << 20))
	}
	return 0
}

// 移除注释
func RemoveNotes(html string) string {
	return regexp.MustCompile(`<!--.*?-->|[^:]//.*|/\*.*?\*/`).ReplaceAllStringFunc(html, func(b string) string {
		if b[1:3] == "//" {
			return b[:1]
		}
		return "\n"
	})
}

// 清理JS注释
func RemoveJSComment(data string) string {
	var result strings.Builder
	inComment := false
	inSingleLineComment := false

	for i := 0; i < len(data); i++ {
		v := data[i]

		if inSingleLineComment && (v == '\n' || v == '\r') {
			inSingleLineComment = false
			result.WriteByte(v)
			continue
		}
		if inComment && v == '*' && i+1 < len(data) && data[i+1] == '/' {
			inComment = false
			i++
			continue
		}
		if inComment || inSingleLineComment {
			continue
		}
		if v == '/' && i+1 < len(data) {
			nextChar := data[i+1]
			if nextChar == '*' {
				inComment = true
				i++
				continue
			} else if nextChar == '/' {
				inSingleLineComment = true
				i++
				continue
			}
		}
		result.WriteByte(v)
	}

	return result.String()
}

// ========== acw_sc__v2 相关 ==========

var findAcwScV2Reg = regexp.MustCompile(`arg1\s*=\s*['"]([0-9A-Fa-f]{40})['"]`)

// CalcAcwScV2 根据 arg1 计算 acw_sc__v2
func CalcAcwScV2(htmlContent string) (string, error) {
	matches := findAcwScV2Reg.FindStringSubmatch(htmlContent)
	if len(matches) != 2 {
		return "", errors.New("无法匹配到 arg1 参数")
	}
	arg1 := matches[1]

	pos := []int{
		15, 35, 29, 24, 33, 16, 1, 38, 10, 9,
		19, 31, 40, 27, 22, 23, 25, 13, 6, 11,
		39, 18, 20, 8, 14, 21, 32, 26, 2, 30,
		7, 4, 17, 5, 3, 28, 34, 37, 12, 36,
	}
	mixed := make([]byte, len(pos))
	for i, p := range pos {
		if p-1 >= len(arg1) {
			return "", fmt.Errorf("位置索引越界: %d", p)
		}
		mixed[i] = arg1[p-1]
	}

	key := "3000176000856006061501533003690027800375"
	result := make([]byte, len(mixed)/2)
	for i := 0; i < len(result); i++ {
		b1, err1 := strconv.ParseUint(string(mixed[i*2:i*2+2]), 16, 8)
		b2, err2 := strconv.ParseUint(key[i*2:i*2+2], 16, 8)
		if err1 != nil || err2 != nil {
			return "", fmt.Errorf("hex 解析失败: %v, %v", err1, err2)
		}
		result[i] = byte(b1 ^ b2)
	}
	return hex.EncodeToString(result), nil
}

// ========== JS 解析相关 ==========

var findDataReg = regexp.MustCompile(`data[:\s]+({[^}]+})`)    // 查找json
var findKVReg = regexp.MustCompile(`'(.+?)':('?([^' },]*)'?)`) // 拆分kv

// 根据key查询js变量
func findJSVarFunc(key, data string) string {
	re := regexp.MustCompile(`var\s+` + regexp.QuoteMeta(key) + `\s*=\s*['"]?([^'"]*)['"]?\s*;`)
	matches := re.FindAllStringSubmatch(data, -1)
	if len(matches) == 0 {
		return ""
	}
	for i := len(matches) - 1; i >= 0; i-- {
		if matches[i][1] != "" {
			return matches[i][1]
		}
	}
	return matches[len(matches)-1][1]
}

var findFunction = regexp.MustCompile(`(?ims)^function[^{]+`)
var findFunctionAll = regexp.MustCompile(`(?is)function[^{]+`)

// 查找所有方法位置
func findJSFunctionIndex(data string, all bool) [][2]int {
	findFunction := findFunction
	if all {
		findFunction = findFunctionAll
	}

	indexs := findFunction.FindAllStringIndex(data, -1)
	fIndexs := make([][2]int, 0, len(indexs))

	for _, index := range indexs {
		if len(index) != 2 {
			continue
		}
		count, data := 0, data[index[1]:]
		for ii, v := range data {
			if v == ' ' && count == 0 {
				continue
			}
			if v == '{' {
				count++
			}

			if v == '}' {
				count--
			}
			if count == 0 {
				fIndexs = append(fIndexs, [2]int{index[0], index[1] + ii + 1})
				break
			}
		}
	}
	return fIndexs
}

// 删除JS全局方法
func removeJSGlobalFunction(html string) string {
	indexs := findJSFunctionIndex(html, false)
	block := make([]string, len(indexs))
	for i, next := len(indexs)-1, len(html); i >= 0; i-- {
		index := indexs[i]
		block[i] = html[index[1]:next]
		next = index[0]
	}
	return strings.Join(block, "")
}

// 根据名称获取方法
func getJSFunctionByName(html string, name string) (string, error) {
	indexs := findJSFunctionIndex(html, true)
	for _, index := range indexs {
		data := html[index[0]:index[1]]
		if regexp.MustCompile(`function\s+` + name + `[()\s]+{`).MatchString(data) {
			return data, nil
		}
	}
	return "", fmt.Errorf("not find %s function", name)
}

// 解析html中的JSON,选择最长的数据
func htmlJsonToMap2(html string) (map[string]string, error) {
	datas := findDataReg.FindAllStringSubmatch(html, -1)
	var sData string
	for _, data := range datas {
		if len(datas) > 0 && len(data[1]) > len(sData) {
			sData = data[1]
		}
	}
	if sData == "" {
		return nil, fmt.Errorf("not find data")
	}
	return jsonToMap(sData, html), nil
}

// 解析html中的JSON
func htmlJsonToMap(html string) (map[string]string, error) {
	datas := findDataReg.FindStringSubmatch(html)
	if len(datas) != 2 {
		return nil, fmt.Errorf("not find data")
	}
	return jsonToMap(datas[1], html), nil
}

func jsonToMap(data, html string) map[string]string {
	var param = make(map[string]string)
	kvs := findKVReg.FindAllStringSubmatch(data, -1)
	for _, kv := range kvs {
		k, v := kv[1], kv[3]
		if v == "" || strings.Contains(kv[2], "'") || IsNumber(kv[2]) {
			param[k] = v
		} else {
			param[k] = findJSVarFunc(v, html)
		}
	}
	return param
}

func IsNumber(str string) bool {
	for _, s := range str {
		if !unicode.IsDigit(s) {
			return false
		}
	}
	return true
}

var findFromReg = regexp.MustCompile(`data : '(.+?)'`) // 查找from字符串

// 解析html中的form
func htmlFormToMap(html string) (map[string]string, error) {
	forms := findFromReg.FindStringSubmatch(html)
	if len(forms) != 2 {
		return nil, fmt.Errorf("not find file sign")
	}
	return formToMap(forms[1]), nil
}

func formToMap(from string) map[string]string {
	var param = make(map[string]string)
	for _, kv := range strings.Split(from, "&") {
		kv := strings.SplitN(kv, "=", 2)[:2]
		param[kv[0]] = kv[1]
	}
	return param
}

var regExpirationTime = regexp.MustCompile(`e=(\d+)`)

func GetExpirationTime(url string) (etime time.Duration) {
	exps := regExpirationTime.FindStringSubmatch(url)
	if len(exps) < 2 {
		return
	}
	timestamp, err := strconv.ParseInt(exps[1], 10, 64)
	if err != nil {
		return
	}
	etime = time.Duration(timestamp-time.Now().Unix()) * time.Second
	return
}

func CookieToString(cookies []*http.Cookie) string {
	if cookies == nil {
		return ""
	}
	cookieStrings := make([]string, len(cookies))
	for i, cookie := range cookies {
		cookieStrings[i] = cookie.Name + "=" + cookie.Value
	}
	return strings.Join(cookieStrings, ";")
}