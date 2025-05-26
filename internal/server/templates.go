package server

import (
	html_template "html/template"

	"github.com/anupcshan/tscloudvpn/cmd/tscloudvpn/assets"
)

var templates = html_template.Must(html_template.New("root").ParseFS(assets.Assets, "*.tmpl"))
