# DE definitions

Request bodies for Terrain (`https://de.cyverse.org/terrain`). They must match the image: change
them together with `latest/Dockerfile` and `VERSION`. None of this has been run against the DE yet
(milestone M2, `docs/verification.md` part 2).

| File | Use |
|---|---|
| `tool.json` | `POST /tools`: the private tool `mesa-rustgis`. |
| `app.json` | `POST /apps/de`: the app **MESA RustGIS**, no parameters. |
| `app-readonly.json` | `POST /apps/de`: **MESA RustGIS (read-only)**, the same tool with `RGIS_READONLY=1`. |
| `app-docs.md` | The app documentation, in the DE's template. |

```bash
T=https://de.cyverse.org/terrain
auth="Authorization: Bearer $TOKEN"   # GET $T/token/keycloak with HTTP Basic auth

tool_id=$(curl -fsS -H "$auth" -H 'Content-Type: application/json' -d @de/tool.json "$T/tools" | jq -r .id)
jq --arg id "$tool_id" '.tools[0].id = $id' de/app.json |
  curl -fsS -H "$auth" -H 'Content-Type: application/json' -d @- "$T/apps/de"
```

- **Public tool** (`POST /admin/tools`, DE administrators): the body is a list and needs an
  implementation block with a contact address:

  ```bash
  jq --arg email "$CONTACT_EMAIL" '{tools: [. + {implementation: {implementor: "MESA / CyVerse",
    implementor_email: $email, test: {input_files: [], output_files: []}}}]}' de/tool.json
  ```

- **Read-only app**: `app-readonly.json` passes `RGIS_READONLY=1` as a hidden parameter of type
  `EnvironmentVariable`. Whether VICE delivers that to the container is not confirmed (row D30 in
  `docs/verification.md`): after launching, `GET /api/v1/session` must report `"readOnly": true`.
  If it does not, set the variable another way and do not offer the app as read-only until it does.
- **Launch option**: the app needs "Mount data store in analysis" on, which is the DE's default
  (row D37 in `docs/verification.md` covers what happens with it off).
- **Versions**: `tool.json` (`version`, `container.image.tag`) and both app files (`version`,
  `tools[0].version`) must equal the `VERSION` file; `pr.yml` checks it. `scripts/upstream.sh
  bump` and `rebuild` change them together.
- **New image version**: add a tool version and an app version labelled with the image tag. Do not
  edit existing ones.
- Do not run vice-app-integrator's Dockerfile validation on this image: it requires GoCommands, the
  GitHub CLI and Node.js, which the image leaves out on purpose.
