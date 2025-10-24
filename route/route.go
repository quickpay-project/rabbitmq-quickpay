package route

import (
	"errors"
	"log"
	"net/http"
	"reflect"
	"strings"
	"unicode"

	controller "github.com/celalsahinaltinisik/controllers"
)

// แปลง URL path เป็นชื่อ method CamelCase เช่น /merchant-keys/update-balance -> MerchantKeysUpdateBalance
func urlToFuncName(path string) string {
	parts := strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '-' })
	for i, part := range parts {
		if len(part) > 0 {
			runes := []rune(part)
			runes[0] = unicode.ToUpper(runes[0])
			parts[i] = string(runes)
		}
	}
	return strings.Join(parts, "")
}

// ตรวจสอบ routing map
func urls(s map[string]string, path string, method string) (bool, string) {
	if m, ok := s[path]; ok && m == method {
		return true, urlToFuncName(path)
	}
	return false, ""
}

// main routing handler
func Routeing() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		routings := map[string]string{
			"/home":             "GET",
			"/online":           "GET",
			"/withdraw":         "POST",
			"/deposit":          "POST",
			"/withdrawauto":     "POST",
			"/depositauto":      "POST",
			"/conswithdraw":     "GET",
			"/consdeposit":      "GET",
			"/conswithdrawauto": "GET",
			"/consdepositauto":  "GET",
		}

		log.Println("📌 Request:", r.URL.Path, r.Method)
		check, funcName := urls(routings, r.URL.Path, r.Method)
		log.Println("➡ Function:", funcName)
		if !check {
			http.Error(w, "404 not found.", http.StatusNotFound)
			return
		}

		m := controller.Functions{}
		if _, err := Call(m, funcName, w, r); err != nil {
			log.Println("❌ Call error:", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
}

// ใช้ reflect call method
func Call(m interface{}, name string, params ...interface{}) (result []reflect.Value, err error) {
	f := reflect.ValueOf(m).MethodByName(name)
	if !f.IsValid() {
		err = errors.New("method not found: " + name)
		return
	}

	if len(params) != f.Type().NumIn() {
		err = errors.New("number of parameters mismatch")
		return
	}

	in := make([]reflect.Value, len(params))
	for i, param := range params {
		in[i] = reflect.ValueOf(param)
	}

	result = f.Call(in)
	return
}
