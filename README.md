# go-tfe-bulk

Perform bulk operations on Terraform Cloud Workspaces.

## Usage

Export an appropriate Terraform Cloud Token:

```shell
export TFE_TOKEN=<token>
```

Now perform some bulk operations:
```shell
# Start new runs for all matching workspaces found:
go run main.go -org myOrg -search dev-eu -action run

# Cancel the current run for all matching workspaces found, if possible:
go run main.go -org myOrg -search dev-eu -action cancel

# Discard the current run for all matching workspaces found, if possible:
go run main.go -org myOrg -search dev-eu -action discard

# Confirm the current run for all matching workspaces found, if possible:
go run main.go -org myOrg -search dev-eu -action confirm

# Cleanup the current run for all matching workspaces found, if possible:
# This will cancel or discard runs until there is only one run remaining, or
# if there is only one run AND the workspace is configured to auto-apply then
# the run will be confirmed
go run main.go -org myOrg -search dev-eu -action cleanup
```

Every command will prompt for confirmation before acting, this can be overridden
with `-assume-yes`:

```shell
go run main.go -org myOrg -search dev-eu -action run -assume-yes
```

The `-search` flag is passed directly to [WorkspaceListOptions](https://pkg.go.dev/github.com/hashicorp/go-tfe@v1.10.0?utm_source=gopls#WorkspaceListOptions):
```
Search string `url:"search[name],omitempty"`
```

If you have disabled Cost Estimation, the status which waits for confirmation
may need to be modified. This only matters for `-action cleanup`:

```shell
go run main.go -org myOrg -search dev-eu -action cleanup -stuck-status planned
```

It's up to you to get the correct status, check the [go-tfe code](https://github.com/hashicorp/go-tfe/blob/main/run.go).
