// SPDX-License-Identifier: AGPL-3.0-or-later

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/JamesonRGrieve/tofu-vyos/internal/vyos"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*objectResource)(nil)
	_ resource.ResourceWithConfigure   = (*objectResource)(nil)
	_ resource.ResourceWithImportState = (*objectResource)(nil)
)

// privateManagedKey names the private-state record holding the last-declared
// `config` JSON — the prune baseline (see Update). Kept out of the schema so it
// never appears in plans.
const privateManagedKey = "managed_config"

// NewObjectResource constructs the generic vyos_config resource.
func NewObjectResource() resource.Resource { return &objectResource{} }

type objectResource struct {
	client *vyos.Client
}

// objectModel is the state/plan shape for vyos_config.
type objectModel struct {
	ID     types.String `tfsdk:"id"`
	Path   types.List   `tfsdk:"path"`
	Config types.String `tfsdk:"config"`
}

func (r *objectResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_config"
}

func (r *objectResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A generic VyOS configuration node addressed by its config `path` " +
			"(a list of path segments, e.g. `[\"interfaces\",\"ethernet\",\"eth1\"]`). " +
			"Covers 100% of the VyOS config tree: any node, from a leaf " +
			"(`[\"system\",\"host-name\"]`) to a whole subtree (`[\"service\",\"dns\",\"forwarding\"]`). " +
			"`config` declares only the keys this resource manages; device-returned keys outside `config` " +
			"are ignored for drift, so a subset declaration imports to 0-diff and never clobbers unmanaged config.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resource id — the `path` segments joined by `/`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"path": schema.ListAttribute{
				ElementType: types.StringType,
				Required:    true,
				MarkdownDescription: "VyOS config path as an ordered list of segments. ForceNew — " +
					"the node identity. E.g. `[\"interfaces\",\"ethernet\",\"eth1\"]`, `[\"system\",\"host-name\"]`.",
				PlanModifiers: []planmodifier.List{requiresReplaceList{}},
			},
			"config": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "JSON subtree of the declared (managed) config at `path`, in the same shape " +
					"`/retrieve showConfig` returns: nested objects for sub-nodes, a string for a single-value leaf, " +
					"an array of strings for a multi-value leaf, and `{}`/`null` for a valueless (tag-present) node. " +
					"State holds the full device subtree; drift is detected only on these declared keys. The subtree " +
					"is flattened into `set` commands on create/update (and removed keys into `delete` commands).",
				PlanModifiers: []planmodifier.String{subsetSuppress{}},
			},
		},
	}
}

func (r *objectResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*vyos.Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data",
			fmt.Sprintf("expected *vyos.Client, got %T", req.ProviderData))
		return
	}
	r.client = client
}

// pathSegments extracts the config path from the model as a Go string slice.
func pathSegments(ctx context.Context, l types.List) ([]string, error) {
	if l.IsNull() || l.IsUnknown() {
		return nil, fmt.Errorf("path is null/unknown")
	}
	var segs []string
	diags := l.ElementsAs(ctx, &segs, false)
	if diags.HasError() {
		return nil, fmt.Errorf("path is not a list of strings")
	}
	return segs, nil
}

func (r *objectResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m objectModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	segs, err := pathSegments(ctx, m.Path)
	if err != nil {
		resp.Diagnostics.AddError("Invalid path", err.Error())
		return
	}
	cmds, err := setCommands(segs, m.Config.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid config", err.Error())
		return
	}
	if err := r.client.Configure(cmds); err != nil {
		resp.Diagnostics.AddError("VyOS configure (set) failed", err.Error())
		return
	}
	m.ID = types.StringValue(strings.Join(segs, "/"))
	// Store the declared config verbatim so create plan/state are consistent;
	// the next refresh (Read) replaces it with the full device subtree.
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
	// Record the declared config in private state so a later Update prunes only
	// genuinely declared-then-removed leaves — `config` in state is overwritten
	// by Read with the full device subtree and must never be the prune baseline.
	resp.Diagnostics.Append(resp.Private.SetKey(ctx, privateManagedKey, []byte(m.Config.ValueString()))...)
}

func (r *objectResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m objectModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	segs, err := pathSegments(ctx, m.Path)
	if err != nil {
		resp.Diagnostics.AddError("Invalid path", err.Error())
		return
	}
	raw, err := r.client.ShowConfig(segs)
	if err != nil {
		if vyos.NotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("VyOS retrieve (showConfig) failed", err.Error())
		return
	}
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "{}" {
		// Path absent / empty subtree — treat as gone.
		resp.State.RemoveResource(ctx)
		return
	}
	compact, err := compactJSON(raw)
	if err != nil {
		resp.Diagnostics.AddError("VyOS read: invalid JSON from device", err.Error())
		return
	}
	m.Config = types.StringValue(unwrapLeafKeyed(segs, compact))
	m.ID = types.StringValue(strings.Join(segs, "/"))
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *objectResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan objectModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	segs, err := pathSegments(ctx, plan.Path)
	if err != nil {
		resp.Diagnostics.AddError("Invalid path", err.Error())
		return
	}
	// set all declared leaves from the new config; delete leaves that were in
	// the prior *declared* config but are no longer declared. The prune baseline
	// is the previously-declared config from private state — NOT `config` in
	// resource state, which Read overwrites with the full device subtree (diffing
	// that would delete every unmanaged device leaf). An absent record (imported
	// resource or pre-upgrade state) prunes nothing, so update is set-only — never
	// destructive to unmanaged config.
	setCmds, err := setCommands(segs, plan.Config.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid config", err.Error())
		return
	}
	priorDeclared, d := req.Private.GetKey(ctx, privateManagedKey)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	delCmds, err := pruneCommands(segs, string(priorDeclared), plan.Config.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid prior config", err.Error())
		return
	}
	cmds := append(delCmds, setCmds...)
	if err := r.client.Configure(cmds); err != nil {
		resp.Diagnostics.AddError("VyOS configure (update) failed", err.Error())
		return
	}
	plan.ID = types.StringValue(strings.Join(segs, "/"))
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	resp.Diagnostics.Append(resp.Private.SetKey(ctx, privateManagedKey, []byte(plan.Config.ValueString()))...)
}

func (r *objectResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m objectModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	segs, err := pathSegments(ctx, m.Path)
	if err != nil {
		resp.Diagnostics.AddError("Invalid path", err.Error())
		return
	}
	err = r.client.Configure([]vyos.Command{{Op: "delete", Path: segs}})
	if err != nil && !vyos.NotFound(err) {
		resp.Diagnostics.AddError("VyOS configure (delete) failed", err.Error())
	}
}

func (r *objectResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Import id is the config path, segments joined by "/" (the same form the
	// computed id takes), e.g. "interfaces/ethernet/eth1". The declared config
	// is seeded to "{}" and populated by the following Read with the full device
	// subtree; the subset plan modifier then reconciles it to 0-diff against the
	// user's config block.
	segs := strings.Split(strings.Trim(req.ID, "/"), "/")
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("path"), segs)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), strings.Join(segs, "/"))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("config"), "{}")...)
}

// ---------------------------------------------------------------------------
// flatten — translate a JSON config subtree (the showConfig shape) into VyOS
// `set` path-arrays. Each leaf becomes one command whose path is the base path
// plus the chain of object keys plus, for value leaves, the value as the final
// segment.
//
// showConfig encodes config nodes as:
//   - nested object        → a config sub-node (recurse into keys)
//   - string               → a single-value leaf      (key, value)
//   - array of strings     → a multi-value leaf        (key, v1), (key, v2)...
//   - {} or null           → a valueless / tag-present node (key)
// ---------------------------------------------------------------------------

// setCommands flattens the declared config subtree at base into `set` commands.
func setCommands(base []string, configJSON string) ([]vyos.Command, error) {
	var v any
	if err := json.Unmarshal([]byte(configJSON), &v); err != nil {
		return nil, fmt.Errorf("`config` must be valid JSON: %w", err)
	}
	leaves := flattenLeaves(base, v)
	cmds := make([]vyos.Command, 0, len(leaves))
	for _, lp := range leaves {
		cmds = append(cmds, vyos.Command{Op: "set", Path: lp})
	}
	return cmds, nil
}

// pruneCommands returns `delete` commands for leaves present in the prior
// declared subtree but absent from the new one. base is the node path.
func pruneCommands(base []string, priorJSON, newJSON string) ([]vyos.Command, error) {
	var pv, nv any
	if err := json.Unmarshal([]byte(priorJSON), &pv); err != nil {
		// Prior state may legitimately hold the full device subtree (post-Read);
		// if it does not parse, skip pruning rather than erroring.
		return nil, nil //nolint:nilerr // best-effort prune; new set commands still apply
	}
	if err := json.Unmarshal([]byte(newJSON), &nv); err != nil {
		return nil, fmt.Errorf("`config` must be valid JSON: %w", err)
	}
	prior := leafSet(flattenLeaves(base, pv))
	current := leafSet(flattenLeaves(base, nv))
	var cmds []vyos.Command
	// Sort for deterministic ordering (stable tests + readable plans).
	keys := make([]string, 0, len(prior))
	for k := range prior {
		if _, still := current[k]; !still {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		cmds = append(cmds, vyos.Command{Op: "delete", Path: prior[k]})
	}
	return cmds, nil
}

// flattenLeaves walks a decoded JSON value rooted at prefix and returns one
// path-array per leaf (set-command path).
func flattenLeaves(prefix []string, v any) [][]string {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			// valueless node (tag present, no children) — set the node itself.
			return [][]string{appendCopy(prefix)}
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var out [][]string
		for _, k := range keys {
			out = append(out, flattenLeaves(appendCopy(prefix, k), t[k])...)
		}
		return out
	case []any:
		if len(t) == 0 {
			return [][]string{appendCopy(prefix)}
		}
		var out [][]string
		for _, e := range t {
			out = append(out, appendCopy(prefix, scalarString(e)))
		}
		return out
	case nil:
		return [][]string{appendCopy(prefix)}
	default:
		return [][]string{appendCopy(prefix, scalarString(t))}
	}
}

// scalarString renders a JSON scalar (string/number/bool) as a VyOS path
// segment. Numbers come back from encoding/json as float64; render integers
// without a trailing ".0".
func scalarString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	default:
		return fmt.Sprintf("%v", t)
	}
}

// leafSet keys a slice of leaf paths by their joined string for set membership.
func leafSet(leaves [][]string) map[string][]string {
	m := make(map[string][]string, len(leaves))
	for _, lp := range leaves {
		m[strings.Join(lp, "\x00")] = lp
	}
	return m
}

// appendCopy returns base with extra appended, without aliasing base's backing
// array (each leaf must own its slice).
func appendCopy(base []string, extra ...string) []string {
	out := make([]string, 0, len(base)+len(extra))
	out = append(out, base...)
	out = append(out, extra...)
	return out
}

// ---------------------------------------------------------------------------
// subset plan modifier — suppress diff when every declared key already matches
// the full device subtree held in prior state. This is what lets a subset
// `config` import/refresh to 0-diff without clobbering unmanaged device config.
// ---------------------------------------------------------------------------

type subsetSuppress struct{}

func (subsetSuppress) Description(context.Context) string {
	return "Suppress diff when all declared config keys already match the device subtree in state."
}
func (subsetSuppress) MarkdownDescription(context.Context) string {
	return (subsetSuppress{}).Description(nil)
}

func (subsetSuppress) PlanModifyString(_ context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	if req.StateValue.IsNull() || req.StateValue.IsUnknown() {
		return // create — nothing to reconcile against
	}
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	// All declared (config) keys already match the device subtree in prior
	// state: keep the full prior subtree and show no diff. Otherwise leave the
	// planned (config) value in place so the drift surfaces as an update.
	if subsetMatches(req.StateValue.ValueString(), req.ConfigValue.ValueString()) {
		resp.PlanValue = req.StateValue
	}
}

// subsetMatches reports whether the config JSON value is a structural subset of
// the prior JSON value: every object key the config declares is present in
// prior with a structurally-equal (recursively subset-matched) value. Scalars
// and arrays must be deep-equal. Invalid JSON on either side returns false so
// the caller falls back to a normal diff.
func subsetMatches(prior, cfg string) bool {
	var p, c any
	if json.Unmarshal([]byte(prior), &p) != nil {
		return false
	}
	if json.Unmarshal([]byte(cfg), &c) != nil {
		return false
	}
	return valueSubset(p, c)
}

// valueSubset reports whether cfg is a subset of prior. Objects are matched
// key-wise and recursively; everything else must be deep-equal.
func valueSubset(prior, cfg any) bool {
	cm, cok := cfg.(map[string]any)
	pm, pok := prior.(map[string]any)
	if cok {
		if !pok {
			return false
		}
		for k, cv := range cm {
			pv, ok := pm[k]
			if !ok || !valueSubset(pv, cv) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(prior, cfg)
}

// unwrapLeafKeyed reconciles VyOS's /retrieve shape with the "subtree below
// path" convention setCommands/flatten use. VyOS returns a value-leaf addressed
// at its own path keyed by its node name — showConfig ["system","host-name"]
// returns {"host-name":"vyos-lab"}, not the bare "vyos-lab" — and a multi-value
// leaf likewise as {"<name>":[...]}. Storing that keyed shape made Read's state
// a level deeper than the declared config, so the subset modifier never matched
// (perpetual drift) and pruneCommands built a duplicated delete path
// (`delete system host-name host-name vyos-lab` → HTTP 400). When the response
// is a single-key object whose key equals the last path segment, unwrap it to
// the value so state matches the config convention and round-trips to 0-diff.
func unwrapLeafKeyed(segs []string, compact string) string {
	if len(segs) == 0 {
		return compact
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(compact), &obj) != nil || len(obj) != 1 {
		return compact
	}
	if v, ok := obj[segs[len(segs)-1]]; ok {
		return string(v)
	}
	return compact
}

// compactJSON re-serializes raw JSON in compact, key-sorted-by-encoder form.
func compactJSON(raw []byte) (string, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	out, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ---------------------------------------------------------------------------
// requiresReplaceList — ForceNew for the `path` list attribute (no built-in
// list RequiresReplace ships in this framework version's stringplanmodifier).
// ---------------------------------------------------------------------------

type requiresReplaceList struct{}

func (requiresReplaceList) Description(context.Context) string {
	return "Changing path forces resource replacement."
}
func (requiresReplaceList) MarkdownDescription(context.Context) string {
	return (requiresReplaceList{}).Description(nil)
}
func (requiresReplaceList) PlanModifyList(_ context.Context, req planmodifier.ListRequest, resp *planmodifier.ListResponse) {
	if req.StateValue.IsNull() || req.PlanValue.IsUnknown() {
		return
	}
	if !req.StateValue.Equal(req.PlanValue) {
		resp.RequiresReplace = true
	}
}
