// SPDX-License-Identifier: AGPL-3.0-or-later

// Package provider implements the vyos OpenTofu/Terraform provider — a native
// client for the VyOS HTTP API. It is generic over the API surface (the
// vyos_config resource/data source address any config path), giving 100%
// feature coverage of the VyOS config tree without per-feature code.
package provider

import (
	"context"

	"github.com/JamesonRGrieve/tofu-vyos/internal/vyos"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ provider.Provider = (*vyosProvider)(nil)

// New returns the provider factory for a given version.
func New(version string) func() provider.Provider {
	return func() provider.Provider { return &vyosProvider{version: version} }
}

type vyosProvider struct {
	version string
}

type providerModel struct {
	Host     types.String `tfsdk:"host"`
	Key      types.String `tfsdk:"key"`
	Insecure types.Bool   `tfsdk:"insecure"`
}

func (p *vyosProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	// Single-token type name -> resources are `vyos_config`, so Terraform's
	// prefix-before-first-underscore inference resolves the local name cleanly
	// (the source address is jamesonrgrieve/vyos).
	resp.TypeName = "vyos"
	resp.Version = p.version
}

func (p *vyosProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Native provider for VyOS routers/firewalls via the VyOS HTTP API " +
			"(`/configure`, `/retrieve`, `/config-file`). API-key authenticated; config-path based.",
		Attributes: map[string]schema.Attribute{
			"host": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Router address (host or host:port), no scheme. The API is served over HTTPS.",
			},
			"key": schema.StringAttribute{
				Required:            true,
				Sensitive:           true,
				MarkdownDescription: "VyOS HTTP API key (`service https api keys id <name> key <key>`).",
			},
			"insecure": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "Skip TLS verification (default true — VyOS ships a self-signed cert). " +
					"Set false only with a trusted cert installed.",
			},
		},
	}
}

func (p *vyosProvider) Configure(_ context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(context.Background(), &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	insecure := true
	if !cfg.Insecure.IsNull() && !cfg.Insecure.IsUnknown() {
		insecure = cfg.Insecure.ValueBool()
	}
	client := vyos.NewClient(vyos.Config{
		Host:     cfg.Host.ValueString(),
		Key:      cfg.Key.ValueString(),
		Insecure: insecure,
	})
	resp.ResourceData = client
	resp.DataSourceData = client
}

func (p *vyosProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{NewObjectResource}
}

func (p *vyosProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{NewObjectDataSource}
}
