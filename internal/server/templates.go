package server

import (
	html_template "html/template"

	"github.com/anupcshan/tscloudvpn/cmd/tscloudvpn/assets"
)

type providerGroup struct {
	Provider string
	Label    string
	Regions  []mappedRegion
}

func groupByProvider(regions []mappedRegion) []providerGroup {
	var groups []providerGroup
	var current *providerGroup

	for _, r := range regions {
		if current == nil || current.Provider != r.Provider {
			groups = append(groups, providerGroup{
				Provider: r.Provider,
				Label:    r.ProviderLabel,
			})
			current = &groups[len(groups)-1]
		}
		current.Regions = append(current.Regions, r)
	}

	return groups
}

var funcMap = html_template.FuncMap{
	"groupByProvider": groupByProvider,
}

var templates = html_template.Must(
	html_template.New("root").Funcs(funcMap).ParseFS(assets.Assets, "*.tmpl"),
)
