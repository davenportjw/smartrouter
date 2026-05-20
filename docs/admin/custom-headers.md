# 🔒 Custom Request Headers Validation

The Smart Router includes a declarative validation engine to enforce custom HTTP header checks on incoming requests without writing Go code.

---

## ⚙️ Registering a Custom Validation Check

### Steps
1. Go to `/admin/headers`.
2. Click **Register Custom Header**.
3. Fill in the form:
   * **Application**: Select a specific Application, or choose `global` to enforce it across all requests.
   * **Header Name**: The exact HTTP header key to inspect (e.g., `X-Client-Version`).
   * **Description**: A brief note explaining what the header tracks.
   * **Required**: Toggle to `true` to make the header mandatory. Requests missing a required header return `HTTP 400 Bad Request`.
   * **Validation Type**:
     * `presence` / `non-empty`: Checks if the header is provided and contains text.
     * `enum`: Restricts the value to a comma-separated list of options (e.g., `ios,android,web`).
     * `regex`: Validates the value against a regular expression (e.g., `^v[0-9]+\.[0-9]+$`).
   * **Validation Pattern**: Provide the enum options or regex pattern.
4. Click **Register Header**.

---

## 💡 Examples

### Example A: RegEx Suffix/Version Validation
* **Header Name**: `X-App-Client-Version`
* **Required**: `true`
* **Validation Type**: `regex`
* **Validation Pattern**: `^v2\.[0-9]+\.[0-9]+$`
* **Result**: Passes `v2.5.1`, but rejects `v1.9.0`.

### Example B: Traffic Source Enum Restriction
* **Header Name**: `X-Traffic-Source`
* **Required**: `true`
* **Validation Type**: `enum`
* **Validation Pattern**: `mobile,desktop`
* **Result**: Only allows `mobile` or `desktop`. Other values fail with:
  ```json
  {
    "error": {
      "code": 400,
      "message": "Header X-Traffic-Source must be one of: mobile,desktop"
    }
  }
  ```

---

## 🗑️ Deleting a Check

1. Go to `/admin/headers`.
2. Click **Delete** on the rule.
3. The validator is deleted immediately.
