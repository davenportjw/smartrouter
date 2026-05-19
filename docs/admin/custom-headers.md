# 🔒 Declarative Custom Headers Verification

The Smart Router includes a declarative validation engine that enforces custom HTTP header checks on incoming requests. This ensures required context headers (like client version, tracking IDs, or source identifiers) exist and match precise parameters before proxying to upstream LLMs.

**No new Go code is required to enforce custom headers.**

---

## ⚙️ User Flow 3: Enforcing Request Validation Rules

### Step 1: Open the Custom Headers Registry
* Navigate to: `/admin/headers` on the dashboard.
* This registry displays all registered validation checks, showing which Application context they bind to, whether they are mandatory (`required`), and their matching parameters.

### Step 2: Register a Custom Validation Check
1. Click **Register Custom Header** in the top right corner.
2. Fill in the creation form:
   - **Application**: Select the Application profile to bind this validation to (or choose `global` to enforce it for all applications running through the proxy).
   - **Header Name**: The exact HTTP header key to inspect (e.g., `X-Client-Version`).
   - **Description**: Brief note outlining what the header tracks.
   - **Required**: Toggle `True` if requests must have this header. If the header is missing, the proxy instantly rejects the call with an `HTTP 400 Bad Request`.
   - **Validation Type**: Choose how to evaluate the header's value:
     - `presence` / `non-empty`: Merely checks if the header contains text.
     - `enum`: Evaluates if the value matches one of your comma-separated options.
     - `regex`: Checks the value against a regular expression pattern.
   - **Validation Pattern**: Configure the pattern based on your validation type choice:
     - For `enum`: `ios,android,web`
     - For `regex`: `^v[0-9]+\.[0-9]+$`
3. Click **Register Header**. The rule compiles and binds dynamically.

---

## 💡 Tactical Examples

Here is how registered headers map behind the scenes:

### Example A: Enforcing a RegEx Version Check
* **Header Name**: `X-App-Client-Version`
* **Required**: `true`
* **Validation Type**: `regex`
* **Validation Pattern**: `^v2\.[0-9]+\.[0-9]+$`
* **Result**:
  - An incoming request with `X-App-Client-Version: v2.5.1` passes.
  - An incoming request with `X-App-Client-Version: v1.9.0` or missing the header fails immediately with `HTTP 400 Bad Request`.

### Example B: Restricting Traffic to Specific Clients
* **Header Name**: `X-Traffic-Source`
* **Required**: `true`
* **Validation Type**: `enum`
* **Validation Pattern**: `mobile,desktop`
* **Result**:
  - Incoming requests must provide `X-Traffic-Source: mobile` or `X-Traffic-Source: desktop` to route.
  - Requests with `X-Traffic-Source: web` are blocked with an error message:
    ```json
    {
      "error": {
        "code": 400,
        "message": "Header X-Traffic-Source must match enum options: mobile,desktop"
      }
    }
    ```

---

## 🗑️ Removing a Validation Check
1. Locate the rule inside `/admin/headers`.
2. Click the red **Delete** button on the target rule row.
3. The validator is deleted instantly and requests are no longer audited against this rule.
