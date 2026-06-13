// SPDX-License-Identifier: AGPL-3.0-or-later

package provider

import (
	"context"
	"fmt"

	"github.com/JamesonRGrieve/tofu-vyos/internal/vyos"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = (*objectDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*objectDataSource)(nil)
)

// NewObjectDataSource constructs the generic vyos_config data source.
func NewObjectDataSource() datasource.DataSource { return &objectDataSource{} }

type objectDataSource struct {
	client *vyos.Client
}

type objectDataModel struct {
	Path     types.List   `tfsdk:"path"`
	Response types.String `tfsdk:"response"`
}

func (d *objectDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_config"
}

func (d *objectDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Read any VyOS configuration subtree by its config `path` (a list of segments) " +
			"via `/retrieve showConfig`. An empty `path` returns the entire configuration.",
		Attributes: map[string]schema.Attribute{
			"path": schema.ListAttribute{
				ElementType:         types.StringType,
				Required:            true,
				MarkdownDescription: "VyOS config path as a list of segments, e.g. `[\"interfaces\",\"ethernet\",\"eth0\"]`. Empty list = whole config.",
			},
			"response": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The config subtree at `path`, as compact JSON (the `data` field of the showConfig response).",
			},
		},
	}
}

func (d *objectDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*vyos.Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("expected *vyos.Client, got %T", req.ProviderData))
		return
	}
	d.client = client
}

func (d *objectDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var m objectDataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var segs []string
	if !m.Path.IsNull() && !m.Path.IsUnknown() {
		resp.Diagnostics.Append(m.Path.ElementsAs(ctx, &segs, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}
	raw, err := d.client.ShowConfig(segs)
	if err != nil {
		resp.Diagnostics.AddError("VyOS retrieve (showConfig) failed", err.Error())
		return
	}
	compact, err := compactJSON(raw)
	if err != nil {
		resp.Diagnostics.AddError("VyOS read: invalid JSON from device", err.Error())
		return
	}
	m.Response = types.StringValue(compact)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}
