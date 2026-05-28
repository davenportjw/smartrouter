# 🌌 Smart Router Documentation

Welcome to the Smart Router documentation. The Smart Router is a Google Cloud Run proxy for API keys, rate limits, and routing of Gemini and Vertex AI workloads.

---

## 🗺️ System Architecture
Learn how the proxy processes requests, validates keys, and calculates costs.
* 📘 **[System Architecture Overview](./architecture/overview.md)**: Data hierarchy, request execution lifecycle, and sliding-window rate limiting.

---

## 👑 Operator & Admin Manuals
Manage all operational tasks using the HTMX dashboard at `/admin`.
* 🗂️ **[Administration Overview](./admin/README.md)**: Overview of the Portal interface and secure Google Firebase Sign-In.
* 🏢 **[Client Organizations Manual](./admin/client-organizations.md)**: Register parent client organizations, edit fallback limits, and manage billing tiers.
* 📱 **[Apps & API Keys Lifecycle](./admin/apps-and-keys.md)**: Register applications, set priority weights, map client tiers, and manage static keys.
* 🔒 **[Custom Request Verification](./admin/custom-headers.md)**: Declare and enforce custom HTTP headers with regex, enum, and presence checks.
* 🌐 **[Routing & Complexity Rules](./admin/routing-rules.md)**: Configure regional failover overrides, fallback targets, and prompt-complexity threshold mappings.
* 📊 **[Performance & Spend Auditing](./admin/metrics-and-costs.md)**: Monitor real-time RPM/TPM traffic, audit API error spikes, and track cost savings.

---

## 🛠️ Quick Start & Operations Guides
Operational runbooks for configuring, running, and integrating with the proxy.
* 💻 **[Local Development Guide](./guides/local-development.md)**: Spin up the router locally with compiler engines and local JSON mocks (`LOCAL_DEV=true`).
* 🚀 **[Cloud Deployment Manual](./guides/cloud-deployment.md)**: Deploy automatically to Google Cloud Run via `deploy.sh` and Terraform.
* 🧠 **[Dynamic Routing Setup](./guides/dynamic-routing.md)**: Set up complexity-based (`gemini-dynamic`) and custom rules-based routing.
* 🔌 **[Client Authentication](./guides/client-integration.md)**: Integrate client applications using API Keys or IAM Service Accounts.
* 💡 **[Using Examples](./guides/using-examples.md)**: Run containerized static examples, IAM service scripts, and Python dynamic routing tests.

---

## 🧠 Engineering Approaches & Compliance
Developer guidelines for modifying the Smart Router codebase safely.
* 🧪 **[TDD Feature Development](./approaches/tdd-feature-development.md)**: Implement proxy features using Test-Driven Development and full in-memory integration tests.
* 🛡️ **[Model Version Compliance](./approaches/model-compliance.md)**: Keep rules and configs compliant with the Gemini 2.5+ series baseline.
