import { triggerPush } from "./delivery.ts";
import { choice, ok, text, type Handler } from "./http.ts";
import { demoPassword, fail } from "./state.ts";

function document(title: string, content: string, script: string) {
  return `<!doctype html><html lang="zh-CN"><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>${title}</title><style>
  :root{font-family:system-ui,-apple-system,"PingFang SC",sans-serif;color:#202a38;background:#f5f7fa;font-size:16px;line-height:1.6}*{box-sizing:border-box}body{margin:0}main{max-width:840px;margin:48px auto;padding:0 24px}header{display:flex;justify-content:space-between;align-items:center;gap:20px;margin-bottom:28px}h1{font-size:24px;margin:0}h2{font-size:18px;margin:0 0 16px}section{background:white;border:1px solid #d8e0e9;border-radius:8px;padding:24px;margin:16px 0}.grid{display:grid;grid-template-columns:1fr 1fr;gap:20px}label{display:block;font-weight:550}input,select{font:inherit;width:100%;height:44px;background:white;border:1px solid #9aa7b7;border-radius:5px;padding:0 10px;margin:8px 0 16px}button,.button{font:inherit;display:inline-flex;align-items:center;justify-content:center;min-height:44px;cursor:pointer;border:1px solid #13765c;border-radius:5px;padding:8px 18px;background:#13765c;color:white;text-decoration:none}button:hover,.button:hover{background:#0d5d47}button:disabled{cursor:wait;opacity:.65}:focus-visible{outline:3px solid #13765c;outline-offset:3px}.danger{background:white;color:#b42318;border-color:#b42318}.danger:hover{background:#fef3f2}p{margin:8px 0}code{font-family:ui-monospace,monospace}a{color:#13765c}#feedback{min-height:28px;margin:16px 0;color:#13765c}#feedback.error{color:#b42318}table{width:100%;border-collapse:collapse}th,td{text-align:left;padding:10px;border-bottom:1px solid #d8e0e9}footer{margin:24px 0} @media(max-width:600px){main{margin:24px auto;padding:0 16px}header{align-items:flex-start;flex-direction:column}.grid{grid-template-columns:1fr}section{padding:18px}}
  </style></head><body><main>${content}<p id="feedback" role="status" aria-live="polite"></p><footer>本地 Mock · 数据只在当前进程内保存</footer></main><script>
  const feedback=document.getElementById('feedback');
  async function command(path,body){const response=await fetch(path,{method:'POST',headers:{'Content-Type':'application/json','X-Orbit-CSRF':'1'},body:JSON.stringify(body)});const result=await response.json();if(!response.ok)throw new Error(result.message||'请求失败');return result;}
  function message(value,error=false){feedback.textContent=value;feedback.className=error?'error':'';}
  async function action(button,fn){button.disabled=true;try{await fn();}catch(error){message(error.message,true);}finally{button.disabled=false;}}
  ${script}</script></body></html>`;
}
export const controlHandler: Handler = (ctx) => {
  const { path, method, store: s, body } = ctx;
  if (path === "/__demo" && method === "GET")
    return ok(
      document(
        "Orbit 场景控制",
        `
    <header><h1>Orbit 场景控制</h1><a class="button" href="${ctx.origin}" target="_blank" rel="noopener">打开管理控制台</a></header>
    <section><h2>演示账号</h2><table><thead><tr><th>账号</th><th>权限</th></tr></thead><tbody><tr><td><code>demo</code></td><td>平台管理员 / 项目 owner</td></tr><tr><td><code>developer</code></td><td>开发者</td></tr><tr><td><code>viewer</code></td><td>只读</td></tr></tbody></table><p>密码：<code>${demoPassword}</code></p></section>
    <section><h2>执行场景</h2><form id="scenario"><div class="grid"><label>下一次构建<select name="nextBuild"><option value="success">成功</option><option value="failed">失败</option><option value="unknown">结果未知</option><option value="hold">持续运行，供取消</option></select></label><label>下一次发布<select name="nextRelease"><option value="success">成功</option><option value="failed">失败</option><option value="unknown">结果未知</option><option value="hold">持续运行，供取消</option></select></label></div><label>运行与入口观测<select name="evidence"><option value="normal">正常</option><option value="dns_mismatch">DNS 不匹配</option><option value="controller_unavailable">控制器不可用</option><option value="certificate_failed">证书未就绪</option><option value="pod_crash">Pod 崩溃</option></select></label><button>应用场景</button></form></section>
    <section><h2>模拟 GitHub Push</h2><form id="push"><label>Pipeline<select name="pipelineId" id="pipeline"></select></label><button>触发自动交付</button></form><p id="run-link"></p></section>
    <section><h2>身份与数据</h2><div class="grid"><button id="expire" type="button">让当前近期认证过期</button><button id="reset" type="button" class="danger">重置演示数据</button></div></section>`,
        `
    async function load(){const response=await fetch('/__demo/state');const data=await response.json();for(const key of ['nextBuild','nextRelease','evidence'])document.forms.scenario.elements[key].value=data[key];const select=document.getElementById('pipeline');select.replaceChildren();for(const pipeline of data.pipelines){const option=document.createElement('option');option.value=pipeline.id;option.textContent=pipeline.name+(pipeline.enabled?'':'（已停用）');select.appendChild(option);}}
    document.forms.scenario.onsubmit=event=>{event.preventDefault();action(event.submitter,async()=>{await command('/__demo/scenario',Object.fromEntries(new FormData(event.target)));message('场景已应用。下一次任务使用所选结果。');});};
    document.forms.push.onsubmit=event=>{event.preventDefault();action(event.submitter,async()=>{const data=await command('/__demo/push',Object.fromEntries(new FormData(event.target)));const link=document.createElement('a');link.href=data.url;link.target='_blank';link.rel='noopener';link.textContent='查看交付运行';document.getElementById('run-link').replaceChildren(link);message('Push 已触发。');await load();});};
    document.getElementById('expire').onclick=event=>action(event.target,async()=>{await command('/__demo/expire',{});message('近期认证已过期。请在控制台尝试敏感操作。');});
    document.getElementById('reset').onclick=event=>{if(!confirm('重置当前 Mock 数据并退出全部演示会话？'))return;action(event.target,async()=>{await command('/__demo/reset',{});await load();document.getElementById('run-link').replaceChildren();message('数据已重置，请刷新控制台并重新登录。');});};
    load().catch(error=>message(error.message,true));`,
      ),
      200,
      { "Content-Type": "text/html; charset=utf-8" },
    );
  if (path === "/__demo/state" && method === "GET")
    return ok({
      nextBuild: s.nextBuild,
      nextRelease: s.nextRelease,
      evidence: s.evidence,
      pipelines: s.pipelines.map((p) => ({
        id: p.pipeline.id,
        name: p.pipeline.name,
        enabled: p.pipeline.enabled,
      })),
      counts: {
        projects: s.projects.length,
        applications: s.applications.length,
        builds: s.builds.length,
        releases: s.releases.length,
        runs: s.runs.length,
      },
    });
  if (path === "/__demo/scenario" && method === "POST") {
    s.nextBuild = choice(
      body,
      "nextBuild",
      ["success", "failed", "unknown", "hold"] as const,
      s.nextBuild,
    );
    s.nextRelease = choice(
      body,
      "nextRelease",
      ["success", "failed", "unknown", "hold"] as const,
      s.nextRelease,
    );
    s.evidence = choice(
      body,
      "evidence",
      [
        "normal",
        "dns_mismatch",
        "controller_unavailable",
        "certificate_failed",
        "pod_crash",
      ] as const,
      s.evidence,
    );
    return ok({ saved: true });
  }
  if (path === "/__demo/push" && method === "POST") {
    const run = triggerPush(s, text(body, "pipelineId"));
    const p = s.pipelines.find(
      (p) => p.pipeline.id === run.run.deliveryPipelineId,
    )!;
    return ok(
      {
        run,
        url: `${ctx.origin}/projects/${p.pipeline.projectId}/applications/${p.pipeline.applicationId}/pipelines/${p.pipeline.id}/runs/${run.run.id}`,
      },
      202,
    );
  }
  if (path === "/__demo/expire" && method === "POST") {
    if (!ctx.session) fail(401, "authentication_required", "请先在控制台登录");
    ctx.session.recentAt = 0;
    return ok({ expired: true });
  }
  if (path === "/__demo/reset" && method === "POST") {
    s.reset();
    return ok({ reset: true });
  }
  if (path === "/__demo/oidc" && method === "GET") {
    const flow = ctx.url.searchParams.get("flow") ?? "";
    const entry =
      s.oidcFlows.get(flow) ??
      fail(404, "flow_not_found", "请从控制台发起模拟 OIDC");
    return ok(
      document(
        "演示 OIDC",
        `<header><h1>演示 OIDC</h1></header><section><form id="oidc">${entry.kind === "login" ? '<label>登录身份<select name="account"><option value="demo">管理员</option><option value="developer">开发者</option><option value="viewer">只读</option><option value="newcomer">新申请人，进入准入流程</option></select></label>' : "<p>确认使用当前演示身份。</p>"}<button>${entry.kind === "bind" ? "绑定身份" : entry.kind === "reauth" ? "验证身份" : "登录"}</button></form></section>`,
        `document.forms.oidc.onsubmit=event=>{event.preventDefault();action(event.submitter,async()=>{const data=await command('/__demo/oidc/complete',{...Object.fromEntries(new FormData(event.target)),flow:${JSON.stringify(flow)}});location.assign(data.next);});};`,
      ),
      200,
      { "Content-Type": "text/html; charset=utf-8" },
    );
  }
};
