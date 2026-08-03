package translator

// adapters_builtin.go —— 内置协议适配器规格（Phase C）
//
// 这里声明的每一份 AdapterSpec 对应一家「非 OpenAI 兼容」的免 Key 提供者。
// 把 provider.format 设成对应的 ID，网关就会用这套规格与上游对话，
// 并把上游的自定义请求/响应形态双向翻译成 OpenAI 形式。
//
// 用户可在数据目录放 adapters.json 覆盖同名 ID 或新增自己的适配器
// （见 LoadAdaptersFile），无需改代码、无需重编、无需发版。
//
// 关于可验证性的说明（重要）：
//
//	下列规格依据各站点公开的前端调用形态编写。开发机出网受限（duckduckgo.com /
//	theoldllm.vercel.app / felo.ai 均连接超时），无法在本地完成端到端实测。
//	因此每份规格都标注了 Note，说明「哪些字段是确定的、哪些需要上机校准」。
//	若上游改版导致字段变化，用户改一段 adapters.json 即可自愈，不必等新版本。

func init() {
	for _, s := range builtinAdapters() {
		RegisterAdapter(s)
	}
}

// AdapterDuckDuckGo / AdapterTheOldLLM / AdapterFelo 为内置适配器的 format 取值常量。
const (
	AdapterDuckDuckGo = "duckduckgo"
	AdapterTheOldLLM  = "theoldllm"
	AdapterFelo       = "felo"
	AdapterMimocode   = "mimocode"
	AdapterTextPlain  = "text-plain"
)

func builtinAdapters() []*AdapterSpec {
	return []*AdapterSpec{
		// ---------------- DuckDuckGo AI Chat ----------------
		// 协议：先 GET {base}/status（带 x-vqd-accept: 1）从**响应头** x-vqd-4 取会话令牌，
		//      再 POST {base}/chat 带上 x-vqd-4，上游以 SSE 逐条推 {"message":"增量文本"}。
		// base_url 应配置为 https://duckduckgo.com/duckchat/v1
		{
			ID:    AdapterDuckDuckGo,
			Label: "DuckDuckGo AI Chat（免 Key）",
			Note: "需 x-vqd-4 会话令牌握手。若上游已升级为 x-vqd-hash-1（浏览器端 JS 计算），" +
				"此适配器会在握手阶段失败，属上游反自动化升级，非网关缺陷。",
			ChatPath: "/chat",
			Method:   "POST",
			Headers: map[string]string{
				"x-vqd-4":      "${token}",
				"Accept":       "text/event-stream",
				"Origin":       "https://duckduckgo.com",
				"Referer":      "https://duckduckgo.com/",
				"User-Agent":   "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
				"Content-Type": "application/json",
			},
			Preflight: &PreflightSpec{
				Method: "GET",
				URL:    "/status",
				Headers: map[string]string{
					"x-vqd-accept": "1",
					"Origin":       "https://duckduckgo.com",
					"Referer":      "https://duckduckgo.com/",
					"User-Agent":   "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
				},
				TokenHeader: "x-vqd-4",
				TTLSeconds:  900,
			},
			Request: RequestSpec{
				Template: `{"model": ${model}, "messages": ${messages}}`,
			},
			Response: ResponseSpec{
				ContentPath: "message",
				ErrorPath:   "type",
			},
			Stream: StreamSpec{
				Mode:       "sse-json",
				DeltaPath:  "message",
				DoneMarker: "[DONE]",
			},
		},

		// ---------------- The Old LLM ----------------
		// 协议：POST {base}/api/chatgpt，Next.js 自定义路由；上游接受 OpenAI 格式请求体、
		// 返回 OpenAI 格式响应（choices.0.message.content / SSE choices.0.delta.content）。
		// 免 Key 机制：每请求现场用 djb2 算法生成 X-Request-Token（参考 OmniRoute executor，
		// 无需浏览器），并固定 X-Client-Version:3.8.4 + 真实 Chrome/149 UA。
		// base_url 应配置为 https://theoldllm.vercel.app
		// 注：上游托管于 Vercel，数据中心 IP 会被风控（403/429），仅家庭/住宅 IP 可用。
		{
			ID:       AdapterTheOldLLM,
			Label:    "The Old LLM（免 Key）",
			Note:     "免 Key：per-request 用 djb2 算法生成 X-Request-Token（无需浏览器）。上游为 OpenAI 兼容（/api/chatgpt），数据中心 IP 会被 Vercel 风控，仅家庭 IP 可用。",
			ChatPath: "/api/chatgpt",
			Method:   "POST",
			TokenGen: "theoldllm",
			Headers: map[string]string{
				"Content-Type":     "application/json",
				"Accept":           "application/json, text/plain, */*",
				"X-Client-Version": "3.8.4",
				"X-Request-Token":  "${token}",
				"User-Agent":       "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36",
			},
			// 上游接受 OpenAI 格式请求体，原样透传（Template 留空 = 透传）。
			Request: RequestSpec{},
			Response: ResponseSpec{
				ContentPath: "choices.0.message.content",
				ErrorPath:   "error.message",
			},
			Stream: StreamSpec{
				Mode:      "sse-json",
				DeltaPath: "choices.0.delta.content",
			},
		},

		// ---------------- Mimocode（小米 MiMo 免费层，免 Key） ----------------
		// 协议：先 POST {base}/api/free-ai/bootstrap（body {"client":"<设备指纹>"}）取 JWT，
		//      再 POST {base}/api/free-ai/openai/chat，带 Authorization: Bearer <JWT> +
		//      X-Mimo-Source: mimocode-cli-free，上游返回标准 OpenAI 格式响应与 SSE。
		// 反滥用闸门：请求体 messages 必须含一条 content 为固定字符串的 system 消息，否则 403，
		//      故用 InjectSystemMarker 在头部幂等注入该标记（已存在则跳过）。
		// 仅 mimo-auto 模型（约 1M 上下文、128K 输出）。JWT 过期（401/403）由网关既有机制
		//      自动 InvalidateToken 并重生，无需额外代码。
		// base_url 应配置为 https://api.xiaomimimo.com（注意：免费层在 /api/free-ai/*，非 /v1）。
		{
			ID:    AdapterMimocode,
			Label: "Mimocode 免费层（小米 MiMo，免 Key）",
			Note: "免 Key 免费 LLM：bootstrap 取 JWT → Bearer 鉴权调用 /api/free-ai/openai/chat。" +
				"反滥用要求 system 消息含固定标记（网关自动注入）。仅 mimo-auto 模型。比 theoldllm 更稳（不依赖 Vercel，无数据中心 IP 风控）。",
			ChatPath: "/api/free-ai/openai/chat",
			Method:   "POST",
			Headers: map[string]string{
				"Content-Type":  "application/json",
				"Accept":        "application/json, text/event-stream, */*",
				"Authorization": "Bearer ${token}",
				"X-Mimo-Source": "mimocode-cli-free",
			},
			Preflight: &PreflightSpec{
				Method: "POST",
				URL:    "/api/free-ai/bootstrap",
				Headers: map[string]string{
					"Content-Type":  "application/json",
					"X-Mimo-Source": "mimocode-cli-free",
				},
				// ${uuid} 作为设备指纹 client 字段（经 renderRaw 渲染）。
				Body:       `{"client":"${uuid}"}`,
				TokenPath:  "jwt",
				TTLSeconds: 2700,
			},
			// 上游接受 OpenAI 格式请求体，原样透传（Template 留空 = 透传）。
			Request: RequestSpec{},
			// 反滥用：强制在 messages 头部注入固定系统提示词（幂等）。
			InjectSystemMarker: "You are MiMoCode, an interactive CLI tool that helps users with software engineering tasks.",
			Response: ResponseSpec{
				ContentPath: "choices.0.message.content",
				ErrorPath:   "error.message",
			},
			Stream: StreamSpec{
				Mode:      "sse-json",
				DeltaPath: "choices.0.delta.content",
			},
		},

		// ---------------- Felo Search ----------------
		// 协议：POST {base}/api/search/threads，SSE 逐条推 {"type":"answer","data":{"text":"累计全文"}}。
		// 注意上游推的是**累计全文**而非增量，故用 full_path 让引擎自动差分。
		// base_url 应配置为 https://felo.ai
		{
			ID:    AdapterFelo,
			Label: "Felo Search（免 Key）",
			Note: "本质是「联网搜索问答」接口而非通用对话补全：会自带检索与引用，" +
				"不适合当通用 LLM 用；无系统提示词与多轮上下文保真。",
			ChatPath: "/api/search/threads",
			Method:   "POST",
			Headers: map[string]string{
				"Content-Type": "application/json",
				"Accept":       "*/*",
				"Origin":       "https://felo.ai",
				"Referer":      "https://felo.ai/",
				"User-Agent":   "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
			},
			Request: RequestSpec{
				Template: `{"query": ${prompt}, "search_uuid": ${uuid}, "search_options": {"langcode": "zh-CN"}, "search_video": false}`,
			},
			Response: ResponseSpec{
				ContentPath: "data.text",
				ErrorPath:   "error",
			},
			Stream: StreamSpec{
				Mode:       "sse-json",
				FullPath:   "data.text",
				DoneMarker: "[DONE]",
			},
		},

		// ---------------- 通用：纯文本回显端点 ----------------
		// 给「POST 一个 prompt、直接返回纯文本」的极简上游用（不少自建/演示站是这种）。
		// 用户只要把 provider.format 设为 text-plain、base_url 指到端点即可。
		{
			ID:     AdapterTextPlain,
			Label:  "通用：纯文本端点",
			Note:   "POST 一个 {prompt, model}，上游直接回纯文本。适用于极简自建端点。",
			Method: "POST",
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
			Request: RequestSpec{
				Template: `{"prompt": ${prompt}, "model": ${model}}`,
			},
			Response: ResponseSpec{
				PlainText: true,
			},
			Stream: StreamSpec{
				Mode: "none",
			},
		},
	}
}
