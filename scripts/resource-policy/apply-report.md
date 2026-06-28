# Container Cap Application Report

Policy: `/run/media/milosvasic/DATA4TB/Projects/TheApp/Containers/scripts/resource-policy/policy.yaml`

Total: 500 service(s) across 45 file(s)

### `HelixLLM/deploy/compose.yaml` — 9 service(s) updated

- helixllm  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/2048p)
- llamacpp  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- qdrant  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/1024p)
- postgres  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/1024p)
- redis  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- kafka  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (3g/2048p)
- prometheus  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- grafana  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- loki  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)

### `HelixLLM/docker-compose.enterprise.yml` — 6 service(s) updated

- helixllm  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/2048p)
- redis  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- chromadb  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (3g/1024p)
- nginx  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (512m/1024p)
- prometheus  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- grafana  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)

### `HelixMemory/docker/docker-compose.yml` — 7 service(s) updated

- letta-server  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- mem0-api  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- cognee-api  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- qdrant  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/1024p)
- neo4j  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/2048p)
- redis  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- postgres  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/1024p)

### `HelixQA/docker-compose.stack.yml` — 8 service(s) updated

- mediamtx  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- nats  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- ollama  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (12g/2048p)
- helixqa-api  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/2048p)
- helixqa-vision  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/2048p)
- prometheus  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- grafana  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- redis  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)

### `LLMsVerifier/docker-compose.messaging.yml` — 6 service(s) updated

- rabbitmq  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- zookeeper  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- kafka  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (3g/2048p)
- schema-registry  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- kafka-ui  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (3g/2048p)
- kafka-init  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (3g/2048p)

### `LLMsVerifier/docker-compose.prod.yml` — 7 service(s) updated

- llm-verifier  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/2048p)
- postgres  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/1024p)
- redis  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- prometheus  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- grafana  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- nginx  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (512m/1024p)
- watchtower  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)

### `LLMsVerifier/docker-compose.yml` — 1 service(s) updated

- llm-verifier  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/2048p)

### `LLMsVerifier/llm-verifier/docker-compose.yml` — 4 service(s) updated

- llm-verifier  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/2048p)
- nginx  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (512m/1024p)
- prometheus  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- grafana  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)

### `MCP/docker-compose.yml` — 39 service(s) updated

- mcp-everything  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-filesystem  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-memory  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-sequential-thinking  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-fetch  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-git  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-time  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-playwright  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (3g/4096p)
- mcp-browserbase  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (3g/4096p)
- mcp-redis  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-mongodb  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-qdrant  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-elasticsearch  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-supabase  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-github  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-sentry  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-heroku  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-cloudflare  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-aws  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-kubernetes  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-slack  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-telegram  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-notion  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-airtable  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-trello  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-atlassian  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-obsidian  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-brave-search  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-perplexity  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-context7  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-firecrawl  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/2048p)
- mcp-omnisearch  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-langchain  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-llamaindex  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-docs  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-microsoft  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- app-redis  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/2048p)
- app-mongo  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/2048p)
- app-qdrant  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/2048p)

### `cli_agents/the-app/docker-compose.helix.yml` — 5 service(s) updated

- the-app  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (3g/2048p)
- postgres  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/1024p)
- redis  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- worker-1  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- worker-2  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)

### `cli_agents/the-app/security/docker-compose.security.yml` — 4 service(s) updated

- sonarqube  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (8g/2048p)
- sonarqube-db  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (8g/2048p)
- snyk  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- security-scanner  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)

### `docker/acp/docker-compose.acp.yml` — 1 service(s) updated

- acp-manager  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)

### `docker/embeddings/docker-compose.embeddings.yml` — 1 service(s) updated

- embeddings  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)

### `docker/formatters/docker-compose.formatters.yml` — 14 service(s) updated

- autopep8  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- yapf  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- sqlfluff  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- rubocop  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- standardrb  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- php-cs-fixer  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- laravel-pint  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- perltidy  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- cljfmt  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- spotless  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- groovy-lint  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- styler  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- air  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- psscriptanalyzer  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)

### `docker/lsp/docker-compose.lsp.yml` — 10 service(s) updated

- lsp-ai  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- lsp-multi  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- lsp-gopls  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- lsp-rust-analyzer  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- lsp-python  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- lsp-typescript  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- lsp-clangd  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- lsp-java  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- lsp-devops  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- lsp-manager  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)

### `docker/mcp/docker-compose.mcp-all-servers.yml` — 34 service(s) updated

- mcp-fetch  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-git  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-time  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-filesystem  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-memory  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-everything  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-sequentialthinking  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-mongodb  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-redis  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-github  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-slack  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-notion  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-trello  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-kubernetes  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-qdrant  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-supabase  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-atlassian  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-browserbase  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (3g/4096p)
- mcp-firecrawl  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/2048p)
- mcp-brave-search  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-playwright  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (3g/4096p)
- mcp-telegram  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-airtable  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-obsidian  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-heroku  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-cloudflare  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-workers  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-perplexity  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-omnisearch  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-context7  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-llamaindex  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-langchain  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-sentry  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-microsoft  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)

### `docker/mcp/docker-compose.mcp-core.yml` — 7 service(s) updated

- mcp-fetch  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-git  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-time  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-filesystem  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-memory  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-everything  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-sequentialthinking  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)

### `docker/mcp/docker-compose.mcp-full.yml` — 72 service(s) updated

- mcp-fetch  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-git  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-time  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-filesystem  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-memory  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-everything  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-sequential-thinking  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-sqlite  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-puppeteer  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (3g/4096p)
- mcp-postgres  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-mongodb  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-redis  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-mysql  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-elasticsearch  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-supabase  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-qdrant  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-chroma  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-pinecone  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-weaviate  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-github  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-gitlab  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-sentry  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-kubernetes  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-docker  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-ansible  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-aws  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-gcp  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-heroku  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-cloudflare  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-vercel  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-workers  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-jetbrains  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-k8s-alt  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-playwright  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (3g/4096p)
- mcp-browserbase  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (3g/4096p)
- mcp-firecrawl  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/2048p)
- mcp-crawl4ai  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-slack  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-discord  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-telegram  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-notion  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-linear  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-jira  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-asana  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-trello  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-todoist  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-monday  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-airtable  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-obsidian  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-atlassian  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-brave-search  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-exa  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-tavily  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-perplexity  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-kagi  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-omnisearch  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-context7  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-llamaindex  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-langchain  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-openai  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-google-drive  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-google-calendar  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-google-maps  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-youtube  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-gmail  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-datadog  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-grafana  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-prometheus  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-stripe  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-hubspot  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-zendesk  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-figma  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)

### `docker/mcp/docker-compose.mcp-servers.yml` — 35 service(s) updated

- mcp-fetch  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-git  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-time  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-filesystem  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-memory  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-everything  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-sequentialthinking  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-redis  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-redis-backend  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-mongodb  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-mongodb-backend  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-supabase  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-qdrant  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-qdrant-backend  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-kubernetes  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-github  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-cloudflare  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-heroku  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-sentry  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-playwright  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (3g/4096p)
- mcp-browserbase  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (3g/4096p)
- mcp-firecrawl  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/2048p)
- mcp-slack  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-telegram  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-notion  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-trello  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-airtable  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-obsidian  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-atlassian  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-brave-search  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-perplexity  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-omnisearch  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-context7  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-llamaindex  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-workers  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)

### `docker/mcp/docker-compose.mcp-services.yml` — 9 service(s) updated

- mcp-redis  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-mongodb  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-elasticsearch  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-qdrant  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-chroma  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-postgres  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-mysql  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-minio  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-browser  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)

### `docker/mcp/docker-compose.mcp.yml` — 18 service(s) updated

- mcp-manager  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-filesystem  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-git  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-memory  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-fetch  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-time  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-sqlite  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-puppeteer  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (3g/4096p)
- mcp-sequential-thinking  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-chroma  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-qdrant  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-figma  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-svgmaker  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-imagesorcery  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (3g/1024p)
- mcp-stable-diffusion  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/1024p)
- stable-diffusion-webui  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- mcp-replicate  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-lsp-tools  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)

### `docker/monitoring/docker-compose.yml` — 3 service(s) updated

- prometheus  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- grafana  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- alertmanager  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (512m/1024p)

### `docker/nvidia-rag/docker-compose.nvidia-rag.yml` — 11 service(s) updated

- etcd  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- minio  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/1024p)
- milvus  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (6g/1024p)
- nemo-extraction  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- nemotron-embedding  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- nemotron-reranker  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- nemotron-generation  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- tika-extraction  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- sentence-embedding  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- ollama-generation  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (12g/2048p)
- app-rag-bridge  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/2048p)

### `docker/rag/docker-compose.rag.yml` — 10 service(s) updated

- qdrant  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/1024p)
- weaviate  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/1024p)
- faiss-server  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- sentence-transformers  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- bge-m3  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- ragatouille  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- hyde-service  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- multi-query  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- reranker  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- rag-manager  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)

### `docker/security/snyk/docker-compose.yml` — 4 service(s) updated

- snyk-deps  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- snyk-code  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- snyk-iac  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- snyk-full  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)

### `docker/security/sonarqube/docker-compose.yml` — 3 service(s) updated

- sonarqube  +memswap_limit,pids_limit,oom_score_adj  (8g/2048p)
- postgres  +memswap_limit,pids_limit,oom_score_adj  (4g/1024p)
- sonar-scanner  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)

### `docker/vision/docker-compose.vision.yml` — 1 service(s) updated

- vision  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)

### `docker-compose-remote.yml` — 3 service(s) updated

- postgres  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/1024p)
- redis  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- chromadb  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (3g/1024p)

### `docker-compose.analytics.yml` — 4 service(s) updated

- superset  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- superset-init  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- superset-worker  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- superset-beat  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)

### `docker-compose.bigdata.yml` — 15 service(s) updated

- zookeeper  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- kafka  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (3g/2048p)
- clickhouse  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (8g/1024p)
- neo4j  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/2048p)
- minio  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/1024p)
- minio-init  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/1024p)
- flink-jobmanager  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- flink-taskmanager  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- flink-historyserver  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- iceberg-rest  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- spark-master  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- spark-worker  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- spark-history  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- qdrant  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/1024p)
- qdrant-init  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/1024p)

### `docker-compose.ci.yml` — 16 service(s) updated

- postgres  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/1024p)
- redis  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- chromadb  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (3g/1024p)
- qdrant  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/1024p)
- minio  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/1024p)
- kafka  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (3g/2048p)
- rabbitmq  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- mockllm  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- oauthmock  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- ci-go  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- emulator  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- ci-mobile  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- ci-web  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- ci-desktop  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- ci-integration  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- ci-reporter  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)

### `docker-compose.helixllm-infra.yml` — 4 service(s) updated

- helixllm-postgres  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/1024p)
- helixllm-redis  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/2048p)
- helixllm-qdrant  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/2048p)
- helixllm-kafka  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/2048p)

### `docker-compose.helixllm.yml` — 7 service(s) updated

- helixllm  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/2048p)
- helixllm-postgres  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/1024p)
- helixllm-redis  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/2048p)
- helixllm-qdrant  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/2048p)
- helixllm-kafka  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/2048p)
- helixllm-llamacpp  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/2048p)
- helixllm-prometheus  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/2048p)

### `docker-compose.integration.yml` — 2 service(s) updated

- postgres-test  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/1024p)
- redis-test  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)

### `docker-compose.memory-infra.yml` — 4 service(s) updated

- qdrant  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/1024p)
- neo4j  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/2048p)
- redis  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- postgres  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/1024p)

### `docker-compose.memory.yml` — 7 service(s) updated

- cognee  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- mem0  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- letta  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- qdrant  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/1024p)
- neo4j  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/2048p)
- redis  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- postgres  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/1024p)

### `docker-compose.messaging.yml` — 6 service(s) updated

- rabbitmq  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- zookeeper  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- kafka  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (3g/2048p)
- schema-registry  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- kafka-ui  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (3g/2048p)
- kafka-init  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (3g/2048p)

### `docker-compose.monitoring.yml` — 11 service(s) updated

- prometheus  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- grafana  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- loki  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- promtail  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- alertmanager  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (512m/1024p)
- node-exporter  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (256m/1024p)
- cadvisor  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (512m/1024p)
- redis-exporter  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- postgres-exporter  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/1024p)
- blackbox-exporter  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- app-exporter  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/2048p)

### `docker-compose.multi-provider.yaml` — 5 service(s) updated

- postgres  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/1024p)
- redis  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- app  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/2048p)
- prometheus  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- grafana  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)

### `docker-compose.production.yml` — 7 service(s) updated

- rabbitmq  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- kafka  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (3g/2048p)
- zookeeper  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- schema-registry  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- kafka-ui  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (3g/2048p)
- traefik  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (512m/1024p)
- redis-cluster  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)

### `docker-compose.protocols.yml` — 43 service(s) updated

- app-mcp  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/2048p)
- mcp-manager  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-filesystem  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-git  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-memory  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-fetch  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-time  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-sqlite  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-postgres  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-puppeteer  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (3g/4096p)
- mcp-sequential-thinking  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-ai-experiment-logger  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-api-debugger  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-design-to-code  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-domain-memory  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-lumera-memory  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-gitmcp  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-huggingface  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-shell  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-workflow  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-health-auditor  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-wiki  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-chroma  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-qdrant  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-slack  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-github  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-linear  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-notion  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-brave-search  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-sentry  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-figma  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-svgmaker  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- mcp-replicate  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- lsp-ai  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- lsp-multi  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- lsp-manager  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- acp-manager  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- embedding-sentence-transformers  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- embedding-bge-m3  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- qdrant  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/1024p)
- rag-manager  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- rag-reranker  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- protocol-discovery  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)

### `docker-compose.security.yml` — 9 service(s) updated

- sonarqube  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (8g/2048p)
- sonarqube-db  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (8g/2048p)
- snyk-scanner  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- trivy-scanner  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- gosec-scanner  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- sonar-scanner  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- semgrep-scanner  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- kics-scanner  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- grype-scanner  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)

### `docker-compose.test-full.yml` — 4 service(s) updated

- postgres-test  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/1024p)
- redis-test  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- app-test  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/2048p)
- test-runner  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)

### `docker-compose.test.yml` — 8 service(s) updated

- mock-llm  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- oauth-mock  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- postgres  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/1024p)
- redis  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- ollama  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (12g/2048p)
- app  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/2048p)
- prometheus  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- grafana  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)

### `docker-compose.yml` — 16 service(s) updated

- postgres  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/1024p)
- redis  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- ollama  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (12g/2048p)
- app  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/2048p)
- prometheus  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- grafana  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (1g/1024p)
- cognee  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- chromadb  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (3g/1024p)
- neo4j  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (4g/2048p)
- memgraph  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- mock-llm  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- langchain-server  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- llamaindex-server  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- guidance-server  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- lmql-server  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)
- sglang  +mem_limit,memswap_limit,pids_limit,oom_score_adj  (2g/1024p)

