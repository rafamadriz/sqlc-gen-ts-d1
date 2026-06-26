# sqlc-gen-ts-d1

https://github.com/voluntas/sqlc-gen-ts-d1-spec

This is a prototype based on the original source above.

## Instructions for use

Works with sqlc v1.19.0 and above.

Add `ts-d1` under the `plugins` section of your `sqlc.json/yaml` file.

The v0.0.0-a release has been regenerated to match the main branch, so it may not work as
expected unless you re-generate the SHA256 hash.

```bash
cat <<EOS
{
    "name": "ts-d1",
    "wasm": {
        "url": "https://github.com/orisano/sqlc-gen-ts-d1/releases/download/v0.0.0-a/sqlc-gen-ts-d1.wasm",
        "sha256": "$(curl -sSL https://github.com/orisano/sqlc-gen-ts-d1/releases/download/v0.0.0-a/sqlc-gen-ts-d1.wasm.sha256)"
    }
}
EOS
```

### Options

You can pass comma-separated `key=value` format strings as plugin options.

* `workers-types-v3=1`: Prevents the output of import statements for v3 of `@cloudflare/workers-types` (default is 0)
* `workers-types=2022-11-30`: Allows you to specify the exact version of `@cloudflare/workers-types` to import for v4 (default is 2022-11-30)

## License
MIT
