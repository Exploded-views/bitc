package config

import (
	"fmt"
	"reflect"
	"strings"
)

// GenerateReference renders docs/config-reference.md from the schema's struct tags (spec 5.4).
func GenerateReference() string {
	var b strings.Builder
	b.WriteString("# bitc configuration reference\n\nGenerated from `internal/config/schema.go` by `go generate ./internal/config`. Do not edit.\n\n")
	b.WriteString("| Key | Class | Default |\n| --- | --- | --- |\n")
	walkRef(&b, reflect.TypeOf(fileSchema{}), "", "")
	b.WriteString("\n## Provider sections (`[providers.<type>]`, decoded by core)\n\n| Key | Default |\n| --- | --- |\n")
	for _, k := range providerCoreKeys {
		fmt.Fprintf(&b, "| `providers.<type>.%s` | %s |\n", k.Key, defaultCell(k.Default))
	}
	return b.String()
}

func walkRef(b *strings.Builder, t reflect.Type, prefix, parent string) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := strings.Split(f.Tag.Get("toml"), ",")[0]
		parts := strings.Split(f.Tag.Get("bitc"), ",")
		class, def := parts[0], ""
		if class == "" {
			class = parent
		}
		for _, p := range parts[1:] {
			if strings.HasPrefix(p, "default=") {
				def = strings.TrimPrefix(p, "default=")
			}
			if p == "required" {
				def = "required"
			}
		}
		key := name
		if prefix != "" {
			key = prefix + "." + name
		}
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		switch {
		case ft.Kind() == reflect.Map && name == "pools":
			walkRef(b, ft.Elem(), key+".<pool>", class)
		case ft.Kind() == reflect.Map && name == "groups" && prefix == "":
			walkRef(b, ft.Elem(), key+".<group>", class)
		case ft.Kind() == reflect.Struct:
			walkRef(b, ft, key, class)
		case ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.Struct:
			walkRef(b, ft.Elem(), key+"[]", class)
		default:
			fmt.Fprintf(b, "| `%s` | %s | %s |\n", key, class, defaultCell(def))
		}
	}
}

func defaultCell(d string) string {
	if d == "" {
		return "unset"
	}
	return "`" + d + "`"
}
