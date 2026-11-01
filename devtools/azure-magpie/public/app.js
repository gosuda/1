var I={refs:null,handoffHead:null};function D(){return I.refs}function se(){let e=I.handoffHead;return I.handoffHead=null,e}function oe(e){I.handoffHead=e}async function K(e){try{let t=await fetch(e,{headers:{accept:"application/json"},cache:"no-store"}),n=await t.json().catch(()=>null);if(!t.ok){let a=n&&typeof n==="object"&&"error"in n&&typeof n.error==="string"?n.error:`HTTP ${t.status}`;return{ok:!1,status:t.status,message:a}}return{ok:!0,value:n}}catch{return{ok:!1,status:0,message:"The network request failed. Check your connection and try again."}}}async function A(){let e=await K("/api/refs");if(!e.ok)return e;let t=e.value,n={repo:t.repo||"gosuda/1",mainSha:t["suda/main"]||"",historySha:t["suda/history"]||"",workspaces:Array.isArray(t.workspaces)?t.workspaces:[],projects:Array.isArray(t.projects)?t.projects:[]};return I.refs=n,{ok:!0,value:n}}function N(){return K("/api/changes")}function re(e){return K(`/api/changes/${encodeURIComponent(e)}`)}function o(e){return String(e??"").replace(/&/g,"&amp;").replace(/</g,"&lt;").replace(/>/g,"&gt;").replace(/"/g,"&quot;").replace(/'/g,"&#39;")}function c(e){return document.getElementById(e)}function v(e,t=10){return(e??"").slice(0,t)}function S(e,t,n=`${t}s`){return`${e} ${e===1?t:n}`}var ie=new Intl.RelativeTimeFormat("en",{numeric:"auto"}),We=[["year",31536000],["month",2592000],["week",604800],["day",86400],["hour",3600],["minute",60]];function Qe(e){let t=Date.parse(e??"");if(Number.isNaN(t))return"unknown";let n=Math.round((t-Date.now())/1000);if(Math.abs(n)<60)return"just now";for(let[a,s]of We)if(Math.abs(n)>=s)return ie.format(Math.round(n/s),a);return ie.format(Math.round(n/60),"minute")}function V(e){let t=new Date(Date.parse(e??""));if(Number.isNaN(t.getTime()))return"";return t.toLocaleString("en-US",{year:"numeric",month:"short",day:"numeric",hour:"2-digit",minute:"2-digit",timeZoneName:"short"})}function E(e){if(!e)return'<span class="muted">unknown</span>';return`<time datetime="${o(e)}" title="${o(V(e))}">${o(Qe(e))}</time>`}function P(e,t="neutral",n=""){return`<span class="chip chip-${t}${n?` ${n}`:""}">${o(e)}</span>`}function ze(e){return((e??"").trim().split(/[\s._-]+/).filter(Boolean).slice(0,2).map((a)=>a[0]).join("")||"?").toUpperCase()}function O(e,t){let n=e||t||"unknown",a=t&&t!==e?` <span class="muted">@${o(t)}</span>`:"";return`<span class="person"><span class="avatar" aria-hidden="true">${o(ze(n))}</span><span>${o(n)}${a}</span></span>`}function L(e,t,n="btn btn-ghost btn-sm"){return`<button type="button" class="${n}" data-copy="${o(e)}" aria-label="Copy ${o(t)}">Copy</button>`}function H(e,t="command"){return`<div class="cmd"><code class="cmd-text">${o(e)}</code>${L(e,t,"btn btn-secondary btn-sm")}</div>`}function m(e,t,n){let a=n?`<strong class="banner-title">${o(n)}</strong> `:"";return`<div class="banner banner-${e}" role="${e==="err"?"alert":"status"}">${a}${t}</div>`}function R(e,t,n=""){return`<div class="empty"><h2 class="empty-title">${o(e)}</h2><div class="empty-body">${t}</div>${n}</div>`}function x(e,t){return`<div class="banner banner-err" role="alert"><strong class="banner-title">${o(e)}</strong> ${o(t)} <button type="button" class="btn btn-secondary btn-sm" data-action="retry">Try again</button></div>`}async function le(e){try{return await navigator.clipboard.writeText(e),!0}catch{return!1}}function J(e){let t=c("status");if(t)t.textContent=e}var Ke=[{prefix:"web/gosuda",mirror:"gosuda/website"},{prefix:"web/securl",mirror:"gosuda/securl"},{prefix:"net/grt",mirror:"gosuda/GRT"},{prefix:"net/ivnp",mirror:"gosuda/IVNP"},{prefix:"net/portalite",mirror:"gosuda/portalite"},{prefix:"ipc/hqq/HQQ",mirror:"gosuda/HQQ"},{prefix:"ipc/hqq/hqq-sqlite3",mirror:"gosuda/hqq-sqlite3"},{prefix:"ipc/hqq/hqq-zstd",mirror:"gosuda/hqq-zstd"},{prefix:"devtools/azure-magpie",mirror:"gosuda/azure-magpie"}],Ve=5;function X(e){if(e.startsWith("land/"))return{ns:"land",tone:"warn"};if(e.startsWith("review/"))return{ns:"review",tone:"info"};return{ns:"ws",tone:"accent"}}function j(e,t,n){return`<article class="stat">
    <p class="stat-label">${o(e)}</p>
    <div class="stat-value">${t}</div>
    <p class="stat-note">${o(n)}</p>
  </article>`}function ce(e,t,n){if(!t)return j(e,'<span class="muted">unavailable</span>',n);let a=`<code class="sha-lg" title="${o(t)}">${o(v(t,12))}</code> ${L(t,`${e} commit`)} <a class="link-external" href="https://github.com/${o(D()?.repo??"gosuda/1")}/tree/${o(t)}" target="_blank" rel="noopener">View<span class="visually-hidden"> on GitHub</span></a>`;return j(e,a,n)}function Je(e){if(!e.ok)return j("Changes",'<span class="muted">unavailable</span>',e.message);let t=e.value.changes.length,n=e.value.truncated?"Partial: GitHub read limit reached. Open Changes to see more.":"Changes with a published patchset.";return j("Changes",`<span class="stat-number">${t}</span>`,n)}function Xe(e){let t=e?e.workspaces.length:null;return j("Workspaces",t===null?'<span class="muted">unavailable</span>':`<span class="stat-number">${t}</span>`,"Branches under refs/ws/* in flight.")}function Ye(e){let t;if(!e.ok)t=x("Couldn't load changes.",e.message);else if(e.value.changes.length===0)t=R("No changes yet","<p>A change appears here after <code>git ws-pr</code> publishes its first patchset. Run it on your workspace branch to start a review.</p>");else t=`<ul class="row-list">${e.value.changes.slice(0,Ve).map(Ze).join("")}</ul>`;return`<section class="card" aria-labelledby="recent-title">
    <header class="card-head">
      <div>
        <h2 id="recent-title">Recent changes</h2>
        <p class="card-lede">A <strong>change</strong> is one unit of work, identified by its <code>Change-Id</code> trailer. Each <code>git ws-pr</code> publishes a <strong>patchset</strong> (PS1, PS2, …) of that change.</p>
      </div>
    </header>
    ${t}
    <p class="card-foot"><a href="/changes">All changes</a></p>
  </section>`}function Ze(e){let t=e.review?` · #${e.review.number} ${e.review.merged?"merged":e.review.state}`:"";return`<li class="row-item">
    <a class="row-title" href="/changes/${o(e.id)}">${o(e.subject)}</a>
    <p class="row-meta"><code>${o(e.id.slice(0,9))}</code> · PS${e.patchsetCount}${o(t)} · ${E(e.updatedAt)}</p>
  </li>`}function de(e,t){let n=t.trim().toLowerCase(),a=e.filter((s)=>!n||s.ref.toLowerCase().includes(n)||s.label.toLowerCase().includes(n)||s.sha.toLowerCase().includes(n));if(e.length===0)return`<tr><td colspan="4">${R("No active workspaces","<p>Start one on your machine with <code>git ws-new &lt;actor&gt;/&lt;task&gt;</code>, then publish it with <code>git ws-save</code>.</p>")}</td></tr>`;if(a.length===0)return`<tr><td colspan="4" class="empty-cell">No workspace matches “${o(t)}”.</td></tr>`;return a.map((s)=>{let{ns:r,tone:d}=X(s.label);return`<tr>
        <td data-label="Namespace">${P(r,d)}</td>
        <td data-label="Reference"><code class="ref-code">${o(s.ref)}</code></td>
        <td data-label="Tip"><span class="sha">${o(v(s.sha))}</span> ${L(s.sha,`tip ${s.label}`)}</td>
        <td data-label="Actions" class="cell-actions">
          <button type="button" class="btn btn-secondary btn-sm" data-action="use-head" data-ref="${o(s.label)}">Use as head</button>
          <a class="btn btn-ghost btn-sm" href="/diff/${o(s.label)}" target="_blank" rel="noopener">Open diff<span class="visually-hidden"> in a new tab</span></a>
        </td>
      </tr>`}).join("")}function et(e){return`<section class="card" id="workspaces" aria-labelledby="workspaces-title">
    <header class="card-head">
      <div>
        <h2 id="workspaces-title">Workspaces</h2>
        <p class="card-lede">Work in progress on gosuda/1, each on its own <code>refs/ws/*</code> branch. Use one as the head of a diff link, or open its diff against the trunk.</p>
      </div>
      <div class="card-tools">
        <label class="visually-hidden" for="ws-filter">Filter workspaces</label>
        <input id="ws-filter" type="search" data-filter="workspaces" placeholder="Filter by actor or task" autocomplete="off" spellcheck="false">
        <button type="button" class="btn btn-secondary" data-action="reload">Refresh</button>
      </div>
    </header>
    <div class="table-wrap">
      <table class="data-table">
        <caption class="visually-hidden">Active workspaces and landing requests</caption>
        <thead><tr><th scope="col">Namespace</th><th scope="col">Reference</th><th scope="col">Tip</th><th scope="col">Actions</th></tr></thead>
        <tbody id="ws-body">${e?de(e.workspaces,""):`<tr><td colspan="4">${x("Couldn't load workspaces.","Reload the page to try again.")}</td></tr>`}</tbody>
      </table>
    </div>
  </section>`}function pe(e){let t=c("ws-body"),n=D();if(t&&n)t.innerHTML=de(n.workspaces,e)}function tt(e){let t=e.trim().replace(/^\/+|\/+$/g,"");return{tree:t?`/tree/${t}`:"/tree",history:t?`/history/${t}`:"/history",blame:t?`/blame/${t}`:"/history"}}function ue(e){let t=tt(e);c("subtree-tree")?.setAttribute("href",t.tree),c("subtree-history")?.setAttribute("href",t.history),c("subtree-blame")?.setAttribute("href",t.blame)}function nt(e){return`<section class="card" id="projects" aria-labelledby="projects-title">
    <header class="card-head">
      <div>
        <h2 id="projects-title">Projects</h2>
        <p class="card-lede">Projects mounted in the monorepo and the public mirror each one exports to. Jump to any subtree of the trunk snapshot or its history.</p>
      </div>
    </header>
    <div class="field">
      <label for="subtree-path">Subtree path</label>
      <p class="hint" id="subtree-hint">Example: ipc/hqq/HQQ or web/gosuda</p>
      <div class="inline-form">
        <input id="subtree-path" type="text" data-field="subtree" placeholder="ipc/hqq/HQQ" aria-describedby="subtree-hint" autocomplete="off" spellcheck="false">
        <a id="subtree-tree" class="btn btn-secondary" href="/tree" target="_blank" rel="noopener">Tree</a>
        <a id="subtree-history" class="btn btn-secondary" href="/history" target="_blank" rel="noopener">History</a>
        <a id="subtree-blame" class="btn btn-ghost" href="/history" target="_blank" rel="noopener">Blame</a>
      </div>
    </div>
    <ul class="project-grid">${(e&&e.projects.length>0?e.projects:Ke).map((a)=>{let s=o(a.prefix),r=o(a.mirror);return`<li class="project">
        <div>
          <p class="project-path"><code>//${s}</code></p>
          <p class="project-mirror muted">${r}</p>
        </div>
        <div class="project-links">
          <a href="/tree/${s}" target="_blank" rel="noopener">Tree</a>
          <a href="/history/${s}" target="_blank" rel="noopener">History</a>
          <a href="https://github.com/${r}" target="_blank" rel="noopener">Mirror<span class="visually-hidden"> on GitHub</span></a>
        </div>
      </li>`}).join("")}</ul>
  </section>`}function he(e){let t=e.paths.trim()||"ipc/hqq/HQQ",n=`${e.actor.trim()||"agent-dalgona"}/${e.task.trim()||"task-402"}`,a=t.split(/\s+/)[0];return["git config include.path ../.gitconfig","git suda-init",`git sparse-checkout set ${t}`,`git ws-new ${n}`,`# Edit and run tests inside ${t}`,`git add -A && git commit -m "feat: update ${a}"`,"git ws-save      # publish the workspace","git ws-pr        # optional: open a review with patchset PS1","git ws-sync && git ws-land"].join(`
`)}function fe(e){let t=he(e),n=c("cli-snippet"),a=c("cli-copy");if(n)n.textContent=t;a?.setAttribute("data-copy",t)}function at(){let t=he({actor:"agent-dalgona",task:"task-402",paths:"ipc/hqq/HQQ web/gosuda"});return`<section class="card" id="cli" aria-labelledby="cli-title">
    <header class="card-head">
      <div>
        <h2 id="cli-title">Workspace commands</h2>
        <p class="card-lede">Copy-ready commands for one workspace, following the reference rules in <code>AGENTS.md</code>. Fill in the fields; the commands update as you type.</p>
      </div>
    </header>
    <div class="form-grid">
      <div class="field">
        <label for="cli-actor">Actor</label>
        <p class="hint" id="cli-actor-hint">Your GitHub login or an agent name, such as agent-dalgona.</p>
        <input id="cli-actor" type="text" data-field="cli" value="agent-dalgona" aria-describedby="cli-actor-hint" autocomplete="off" spellcheck="false">
      </div>
      <div class="field">
        <label for="cli-task">Task</label>
        <p class="hint" id="cli-task-hint">A short id, such as task-402 or fix-header.</p>
        <input id="cli-task" type="text" data-field="cli" value="task-402" aria-describedby="cli-task-hint" autocomplete="off" spellcheck="false">
      </div>
      <div class="field">
        <label for="cli-paths">Sparse checkout</label>
        <p class="hint" id="cli-paths-hint">Space-separated google3 paths.</p>
        <input id="cli-paths" type="text" data-field="cli" value="ipc/hqq/HQQ web/gosuda" aria-describedby="cli-paths-hint" autocomplete="off" spellcheck="false">
      </div>
    </div>
    <div class="snippet">
      <pre id="cli-snippet" class="code-block" tabindex="0" aria-label="Workspace commands">${o(t)}</pre>
      <div class="snippet-actions">${L(t,"workspace commands","btn btn-secondary")}</div>
    </div>
  </section>`}async function ge(){let[e,t]=await Promise.all([A(),N()]),n=e.ok?e.value:null,a=e.ok?"":m("err",`Trunk and workspace state could not be loaded: ${e.message}`,"Couldn't reach gosuda/1."),s="Parentless snapshot of the monorepo. Workspaces start from it.";return`
    <header class="page-head">
      <div>
        <p class="eyebrow">Overview</p>
        <h1 tabindex="-1">Overview</h1>
        <p class="lead">The state of gosuda/1 at a glance: the trunk snapshot, recent changes, and the workspaces in flight.</p>
      </div>
    </header>
    ${a}
    <section class="stat-grid" aria-label="At a glance">
      ${ce("suda/main",n?.mainSha,"Parentless snapshot of the monorepo. Workspaces start from it.")}
      ${ce("suda/history",n?.historySha,"Linear, lander-signed history for blame and log.")}
      ${Je(t)}
      ${Xe(n)}
    </section>
    ${Ye(t)}
    ${et(n)}
    ${nt(n)}
    ${at()}
  `}function st(e,t){if(!e)return t?`<span class="muted" title="${o(t)}">Unknown</span>`:'<span class="muted">No review yet</span>';let n=e.merged?"ok":e.state==="open"?"info":"neutral",a=e.merged?"merged":e.state;return`<a href="${o(e.url)}" target="_blank" rel="noopener">#${e.number}<span class="visually-hidden"> ${o(e.title)} on GitHub</span></a> ${P(a,n)}`}function ot(e,t){let n=[e.subject,e.id,e.author.name,e.author.login,e.review?`#${e.review.number}`:""].filter(Boolean).join(" ").toLowerCase();return`<tr data-search="${o(n)}">
    <td data-label="Change"><a class="row-title" href="/changes/${o(e.id)}">${o(e.subject)}</a></td>
    <td data-label="Change-Id"><span class="mono" title="${o(e.id)}">${o(e.id.slice(0,9))}…</span> ${L(e.id,"Change-Id")}</td>
    <td data-label="Author">${O(e.author.name,e.author.login)}</td>
    <td data-label="Patchsets">${S(e.patchsetCount,"patchset")}</td>
    <td data-label="Review">${st(e.review,t)}</td>
    <td data-label="Updated">${E(e.updatedAt)}</td>
  </tr>`}function me(e){let t=[];if(e.truncated&&e.note)t.push(m("warn",e.note,"Partial list."));else if(e.note)t.push(m("info",e.note));if(e.reviewUnavailable)t.push(m("info",e.reviewUnavailable,"Reviews hidden."));return t.join("")}function rt(e){if(e.changes.length===0)return`${me(e)}${R("No changes yet",`<p>A change appears here once <code>git ws-pr</code> publishes its first patchset from a workspace branch. The first run also opens a review on GitHub.</p>
      <p>Run this on your workspace branch:</p>${H("git ws-pr","git ws-pr")}`)}`;let t=e.changes.map((n)=>ot(n,e.reviewUnavailable)).join("");return`${me(e)}
    <div class="toolbar">
      <label class="visually-hidden" for="changes-filter">Filter changes</label>
      <input id="changes-filter" type="search" data-filter="changes" placeholder="Filter changes" aria-describedby="changes-count" autocomplete="off" spellcheck="false">
      <p class="toolbar-count" id="changes-count" aria-live="polite">${S(e.changes.length,"change")}</p>
    </div>
    <section class="card card-flush" aria-label="Changes">
      <div class="table-wrap">
        <table class="data-table changes-table">
          <caption class="visually-hidden">Changes, newest first</caption>
          <thead>
            <tr>
              <th scope="col">Change</th>
              <th scope="col">Change-Id</th>
              <th scope="col">Author</th>
              <th scope="col">Patchsets</th>
              <th scope="col">Review</th>
              <th scope="col">Updated</th>
            </tr>
          </thead>
          <tbody id="changes-body">${t}</tbody>
        </table>
      </div>
    </section>`}async function be(){let e=await N();return`<header class="page-head">
      <div>
        <p class="eyebrow">Changes</p>
        <h1 tabindex="-1">Changes</h1>
        <p class="lead">A <strong>change</strong> is one unit of work, identified by its <code>Change-Id</code> trailer. Each time you run <code>git ws-pr</code>, a new <strong>patchset</strong> is published: PS1 is the first version, PS2 the next, and so on.</p>
      </div>
      <div class="page-actions">
        <button type="button" class="btn btn-secondary" data-action="reload">Refresh</button>
      </div>
    </header>
    ${e.ok?rt(e.value):x("Couldn't load changes.",`${e.message} Check that magpie can read gosuda/1, then try again.`)}`}function ve(e){let t=e.trim().toLowerCase(),n=document.querySelectorAll("#changes-body tr[data-search]"),a=0;n.forEach((r)=>{let d=!t||(r.dataset.search??"").includes(t);if(r.hidden=!d,d)a+=1});let s=c("changes-count");if(s)s.textContent=t?`${a} of ${n.length} shown`:S(n.length,"change")}var we=/^@@ -\d+(?:,\d+)? \+\d+(?:,\d+)? @@/;function Y(e){return we.test(e)?e.replace(we,"@@ @@"):e}function ye(e){return e.split(`
`).map(Y).join(`
`)}function $e(e){let t=new Map;for(let n of e)t.set(n.filename,n);return t}function it(e,t){if(e.patch===null||t.patch===null)return"unknown";if(e.status!==t.status||(e.previous_filename??null)!==(t.previous_filename??null))return"changed";return ye(e.patch)===ye(t.patch)?"identical":"changed"}function ke(e,t){let n=$e(e),a=$e(t),s=[...new Set([...n.keys(),...a.keys()])].sort(),r={identical:0,changed:0,added:0,dropped:0,unknown:0},d=s.map((i)=>{let u=n.get(i)??null,l=a.get(i)??null,p;if(u&&l)p=it(u,l);else if(l)p="added";else p="dropped";r[p]+=1;let h=null,g=null;if(p==="unknown")g="Patch text is not available for this file (binary, too large, or not returned by GitHub).";else if(p==="changed"&&u?.patch!=null&&l?.patch!=null){let b=ct(u.patch,l.patch);if("lines"in b)h=b.lines;else g=b.tooLarge}return{filename:i,classification:p,older:u,newer:l,lines:h,note:g}});return{patchIdentical:d.every((i)=>i.classification==="identical"),files:d,counts:r}}function lt(e,t,n){let a=new Map,s=(f)=>{let w=a.get(f);if(w===void 0)w=a.size,a.set(f,w);return w},r=Int32Array.from(e,s),d=Int32Array.from(t,s),i=r.length,u=d.length,l=0;while(l<i&&l<u&&r[l]===d[l])l++;let p=0;while(p<i-l&&p<u-l&&r[i-1-p]===d[u-1-p])p++;let h=i-l-p,g=u-l-p;if(h*g>n)return null;let b=g+1,C=new Uint32Array((h+1)*b);for(let f=h-1;f>=0;f--)for(let w=g-1;w>=0;w--)C[f*b+w]=r[l+f]===d[l+w]?C[(f+1)*b+w+1]+1:Math.max(C[(f+1)*b+w],C[f*b+w+1]);let T=[];for(let f=0;f<l;f++)T.push({op:"same",ai:f,bi:f});let y=0,k=0;while(y<h&&k<g)if(r[l+y]===d[l+k])T.push({op:"same",ai:l+y,bi:l+k}),y++,k++;else if(C[(y+1)*b+k]>=C[y*b+k+1])T.push({op:"del",ai:l+y,bi:-1}),y++;else T.push({op:"add",ai:-1,bi:l+k}),k++;while(y<h)T.push({op:"del",ai:l+y,bi:-1}),y++;while(k<g)T.push({op:"add",ai:-1,bi:l+k}),k++;for(let f=0;f<p;f++)T.push({op:"same",ai:i-p+f,bi:u-p+f});return T}function ct(e,t,n=2500000){let a=e.split(`
`),s=t.split(`
`),r=lt(a.map(Y),s.map(Y),n);if(r===null)return{tooLarge:`Too large for a line-by-line diff (${a.length} and ${s.length} lines). Compare the two patches on GitHub instead.`};return{lines:r.map(({op:i,ai:u,bi:l})=>{if(i==="del")return{op:i,text:a[u],olderLine:u+1,newerLine:null};if(i==="add")return{op:i,text:s[l],olderLine:null,newerLine:l+1};return{op:i,text:s[l],olderLine:u+1,newerLine:l+1}})}}function Le(e,t=3){let n=e.map(()=>!1);e.forEach((r,d)=>{if(r.op==="same")return;let i=Math.max(0,d-t),u=Math.min(e.length-1,d+t);for(let l=i;l<=u;l++)n[l]=!0});let a=[],s=0;while(s<e.length)if(n[s]){let r=[];while(s<e.length&&n[s])r.push(e[s]),s++;a.push({kind:"lines",lines:r})}else{let r=0;while(s<e.length&&!n[s])r++,s++;a.push({kind:"gap",hidden:r})}return a}function xe(e){let t=[...e].sort((n,a)=>n.filename<a.filename?-1:n.filename>a.filename?1:0);return{files:t,additions:t.reduce((n,a)=>n+(a.additions??0),0),deletions:t.reduce((n,a)=>n+(a.deletions??0),0),unavailable:t.filter((n)=>n.patch===null).length}}function Ce(e){return e.split(`
`).map((t,n)=>{let a="context";if(t.startsWith("@@"))a="hunk";else if(t.startsWith("+"))a="add";else if(t.startsWith("-"))a="del";else if(t.startsWith("\\"))a="meta";return{kind:a,text:t,line:n+1}})}var dt=2000,B=null,q=new Map;async function Pe(e){let t=await re(e);if(!t.ok){if(B=null,t.status===404)return R("No change found",`<p>Nothing matches <code>${o(e)}</code> on gosuda/1. A change appears once <code>git ws-pr</code> has published a patchset for it.</p>
        <p class="muted">Embargoed work is never listed here.</p>
        <p><a href="/changes">Back to all changes</a></p>`);return`${x("Couldn't load this change.",t.message)}<p><a href="/changes">Back to all changes</a></p>`}let n=t.value;B=n;let s=n.patchsets[n.patchsets.length-1].number,r=s>1?s-1:"base";return`${pt(n)}
    ${ut(n)}
    ${n.reviewUnavailable?m("info",n.reviewUnavailable,"Review status hidden."):""}
    ${ht(n)}
    ${bt(n)}
    ${vt(n,String(r),String(s))}
    ${wt(n,String(r),String(s))}`}function pt(e){return`<nav class="crumbs" aria-label="Breadcrumb"><ol>
    <li><a href="/changes">Changes</a></li>
    <li aria-current="page"><code>${o(e.id.slice(0,9))}…</code></li>
  </ol></nav>`}function ut(e){let t=e.patchsets[0],n=e.review,a=n?`<a href="${o(n.url)}" target="_blank" rel="noopener">Review #${n.number}<span class="visually-hidden"> on GitHub</span></a> ${P(n.merged?"merged":n.state,n.merged?"ok":n.state==="open"?"info":"neutral")}`:'<span class="muted">No review yet. Run <code>git ws-pr</code> to open one.</span>';return`<header class="page-head">
    <div>
      <p class="eyebrow">Change · ${S(e.patchsets.length,"patchset")}</p>
      <h1 tabindex="-1">${o(e.subject)}</h1>
      <dl class="meta">
        <div><dt>Change-Id</dt><dd class="id-line"><code class="mono-wrap">${o(e.id)}</code> ${L(e.id,"Change-Id")}</dd></div>
        <div><dt>Review</dt><dd>${a}</dd></div>
        <div><dt>Author</dt><dd>${O(t.author.name,null)}</dd></div>
        <div><dt>Updated</dt><dd>${E(e.updatedAt)}</dd></div>
      </dl>
    </div>
  </header>`}function ht(e){let t=e.review?`Or merge review #${e.review.number} on GitHub. The lander reads the merge from GitHub and judges the approvals on the merged head.`:"Or merge the review on GitHub once it exists.";return`<section class="card callout" aria-labelledby="landing-title">
    <h2 id="landing-title">Landing</h2>
    <p>An approval counts for the commit it was given on, and for a later patchset that is a clean rebase of that commit (the same patch on a newer base). Any other edit needs a new approval. The lander decides.</p>
    <p>From a workspace that has this change checked out, land the latest patchset with:</p>
    ${H("git ws-sync && git ws-land","landing commands")}
    <p class="muted">${o(t)}</p>
  </section>`}function Ee(e,t){return e.approvals.filter((n)=>n.patchset===t)}function ft(e,t){let n=new Set(e.approvals.map((a)=>a.login));return e.reviews.filter((a)=>a.state==="CHANGES_REQUESTED"&&a.commitId===t.sha&&!n.has(a.login)).map((a)=>a.login)}function gt(e,t){let n=`https://github.com/${e.repo}`,a=Ee(e,t.number),s=t.carriesTo.map((h)=>`PS${h}`),r=a.map((h)=>`<span class="badge badge-ok" title="Approved ${o(V(h.submittedAt))}">✓ ${o(h.login)}</span>`);if(a.length>0&&s.length>0)r.push(`<span class="badge badge-carry" title="Estimate: this patchset's patch is identical to ${o(s.join(", "))}. The lander decides.">carries to ${o(s.join(", "))}</span>`);for(let h of ft(e,t))r.push(`<span class="badge badge-warn">${o(h)} requested changes</span>`);let d=t.files,i=d?d.reduce((h,g)=>h+(g.additions??0),0):0,u=d?d.reduce((h,g)=>h+(g.deletions??0),0):0,l=t.error?`<p class="row-error">${o(t.error)}</p>`:"",p=t.rootSha&&!t.error?`<a href="${n}/compare/${o(t.rootSha)}...${o(t.sha)}" target="_blank" rel="noopener">Patch on GitHub<span class="visually-hidden"> for PS${t.number}</span></a>`:"";return`<tr>
    <td data-label="Patchset"><strong>PS${t.number}</strong></td>
    <td data-label="Commit">
      <div class="cell-stack">
        <span><code title="${o(t.sha)}">${o(v(t.sha))}</code> ${L(t.sha,`PS${t.number} commit`)}</span>
        <span class="row-sub">${o(t.subject)}</span>
        ${l}
      </div>
    </td>
    <td data-label="Base">${t.rootSha?`<code title="${o(t.rootSha)}">${o(v(t.rootSha))}</code>`:'<span class="muted">unknown</span>'}</td>
    <td data-label="Published">${E(t.date)}</td>
    <td data-label="Files">${d?`${d.length}${t.filesTruncated?"+":""}`:'<span class="muted">—</span>'} <span class="cell-links">${p}</span></td>
    <td data-label="+ / −" class="nums"><span class="add-count">+${i}</span> <span class="del-count">−${u}</span></td>
    <td data-label="Approvals"><div class="badges">${r.join("")||'<span class="muted">none yet</span>'}</div></td>
  </tr>`}function mt(e){let t=e.approvals.filter((a)=>a.patchset===null);if(t.length===0)return"";let n=t.map((a)=>`${a.login} (${v(a.commitId)})`).join(", ");return`<p class="card-foot">Approvals on commits that are not published patchsets: ${o(n)}.</p>`}function bt(e){let t=e.patchsets.map((n)=>gt(e,n)).join("");return`<section class="card card-flush" aria-labelledby="patchsets-title">
    <header class="card-head pad">
      <div>
        <h2 id="patchsets-title">Patchsets</h2>
        <p class="card-lede">Each patchset is the commit range <code>root..tip</code> on the trunk snapshot it was based on, oldest first. Each approval is bound to the commit the reviewer approved.</p>
        <p class="card-lede"><strong>Approval carry-over</strong> is an estimate: a later patchset that is a clean rebase of an approved one (the same patch on a newer base) would keep that approval. Any other edit needs a new approval. The lander decides.</p>
      </div>
    </header>
    <div class="table-wrap">
      <table class="data-table patchset-table">
        <caption class="visually-hidden">Patchsets of ${o(e.id)}, oldest first</caption>
        <thead>
          <tr>
            <th scope="col">PS</th>
            <th scope="col">Commit</th>
            <th scope="col">Base</th>
            <th scope="col">Published</th>
            <th scope="col">Files</th>
            <th scope="col">+ / −</th>
            <th scope="col">Approvals</th>
          </tr>
        </thead>
        <tbody>${t}</tbody>
      </table>
    </div>
    ${mt(e)}
  </section>`}function Te(e,t,n){let a=n?`<option value="base"${t==="base"?" selected":""}>Base (trunk snapshot)</option>`:"",s=e.patchsets.map((r)=>`<option value="${r.number}"${String(r.number)===t?" selected":""}>PS${r.number} · ${o(v(r.sha,8))}</option>`).join("");return a+s}function vt(e,t,n){return`<section class="card" id="compare" aria-labelledby="compare-title">
    <header class="card-head">
      <div>
        <h2 id="compare-title">Compare patchsets</h2>
        <p class="card-lede">See what changed between two patchsets. Choose <strong>Base</strong> to see one patchset's patch against the trunk snapshot it was based on.</p>
      </div>
    </header>
    <div class="compare-controls">
      <div class="field">
        <label for="cmp-from">From</label>
        <select id="cmp-from" data-compare="from">${Te(e,t,!0)}</select>
      </div>
      <div class="field">
        <label for="cmp-to">To</label>
        <select id="cmp-to" data-compare="to">${Te(e,n,!1)}</select>
      </div>
    </div>
    <div id="cmp-result" class="compare-result" aria-live="polite">${Re(e,t,n)}</div>
  </section>`}function wt(e,t,n){return`<section class="card" aria-labelledby="cli-title">
    <header class="card-head">
      <div>
        <h2 id="cli-title">From the command line</h2>
        <p class="card-lede">Commands for the patchsets selected above.</p>
      </div>
    </header>
    <div id="cmp-cli" class="stack">${He(e,t,n)}</div>
  </section>`}function He(e,t,n){let a=_(e,n);if(!a)return"";let s=_(e,t),r=s?`git ws-psdiff ${s.number} ${a.number}`:null,d=`git suda-ws fetch ws '+refs/ws/cl/${e.id}/*:refs/remotes/ws/ws/cl/${e.id}/*'`,i=null;if(s&&s.rootSha&&a.rootSha)i=`git range-diff ${s.rootSha}..${s.sha} ${a.rootSha}..${a.sha}`;else if(!s&&a.rootSha)i=`git diff ${a.rootSha}..${a.sha}`;let u=r?`<p class="cli-label">On this change's branch</p>
    ${H(r,"ws-psdiff command")}`:"",l=r?"Use the first when this change's branch is checked out; from any other clone, fetch the patchsets once and compare them.":"Fetch the patchsets once, then show this patchset's patch against the trunk snapshot it was based on.";return`${u}
    <p class="cli-label">From any clone</p>
    ${H(d,"fetch command")}
    ${i?H(i,i.startsWith("git diff")?"diff command":"range-diff command"):""}
    <p class="hint">${l}</p>`}function _(e,t){if(t==="base")return null;let n=Number(t);return e.patchsets.find((a)=>a.number===n)??null}function Re(e,t,n){q.clear();let a=_(e,n);if(!a)return m("info","Choose a patchset to compare.");if(t==="base")return $t(e,a);let s=_(e,t);if(!s)return m("info","Choose a patchset to compare.");if(s.number===a.number)return m("info","Pick two different patchsets to see what changed between them.");for(let r of[s,a])if(!r.files)return m("warn",`PS${r.number} can't be compared: ${r.error??"its patch is not available"}.`);return kt(e,s,a)}function yt(e){if(e==="added")return"ok";if(e==="removed")return"err";if(e==="renamed")return"info";return"neutral"}function $t(e,t){let n=xe(t.files??[]),a=n.files,s=a.map((i)=>({name:i.previous_filename?`${i.previous_filename} → ${i.filename}`:i.filename,label:i.status,tone:yt(i.status),additions:i.additions??0,deletions:i.deletions??0,note:i.patch===null?"Patch text is not available (binary or too large). Open it on GitHub.":null,open:a.length<=3,body:()=>U(i)})),r=t.rootSha?`the trunk snapshot ${v(t.rootSha)}`:"the trunk snapshot",d=[t.rootSha?`<a href="https://github.com/${o(e.repo)}/compare/${o(t.rootSha)}...${o(t.sha)}" target="_blank" rel="noopener">Patch on GitHub (root…tip)</a>`:""];return`${m("info",`PS${t.number} against ${r}: ${S(a.length,"file")}, <span class="add-count">+${n.additions}</span> <span class="del-count">−${n.deletions}</span>.`)}
  ${Ae(d)}
  ${Me(s)}`}function kt(e,t,n){let a=ke(t.files??[],n.files??[]),{counts:s}=a,r=Ee(e,t.number),d=[`<a href="https://github.com/${o(e.repo)}/compare/${o(t.sha)}..${o(n.sha)}" target="_blank" rel="noopener">Compare PS${t.number} → PS${n.number} on GitHub (two-dot)</a>`],i;if(a.patchIdentical){let p=r.length>0?`Patch identical: approvals on PS${t.number} carry over to PS${n.number}.`:`Patch identical: PS${t.number} and PS${n.number} make the same change.`;i=m("ok",`${p} <span class="muted">An estimate from comparing patch text; the lander decides.</span>`)}else{let p=a.files.filter((g)=>g.classification!=="identical").length,h=[`${s.changed} changed`,`${s.added} added`,`${s.dropped} dropped`,`${s.identical} identical`];if(s.unknown>0)h.push(`${s.unknown} without patch text`);i=m("info",`${S(p,"file")} changed between PS${t.number} and PS${n.number}: ${h.join(", ")}.`)}let u=a.files.map((p)=>Lt(p)),l=a.files.some((p)=>p.lines!==null)?'<p class="diff-legend">Legend: the outer marker (+ or −) shows what changed between the two patchsets; the sign inside a line is part of the patch itself.</p>':"";return`${i}${Ae(d)}${l}${Me(u)}`}function Lt(e){let t=e.newer??e.older,n=t?.additions??0,a=t?.deletions??0,s={name:e.filename,additions:n,deletions:a};switch(e.classification){case"identical":return{...s,label:"identical",tone:"ok",note:null,open:!1,body:()=>e.newer?.patch?U(e.newer):""};case"changed":return{...s,label:"changed",tone:"warn",note:e.note,open:!0,body:()=>e.lines?Tt(e.lines):F(e.note??"No line-level diff.")};case"added":return{...s,label:"added",tone:"ok",note:"Only in the newer patchset.",open:!1,body:()=>e.newer?.patch?U(e.newer):F("No patch text.")};case"dropped":return{...s,label:"dropped",tone:"err",note:"Only in the older patchset.",open:!1,body:()=>e.older?.patch?U(e.older):F("No patch text.")};case"unknown":return{...s,label:"no patch text",tone:"neutral",note:e.note,open:!1,body:()=>F(e.note??"Patch text is not available.")}}}function Ae(e){let t=e.filter(Boolean);return t.length>0?`<p class="compare-links">${t.join(" · ")}</p>`:""}function Me(e){if(e.length===0)return m("info","No files to show.");let t=e.map((a,s)=>`<li><a href="#diff-${s}">${P(a.label,a.tone)} <span class="mono-wrap">${o(a.name)}</span></a></li>`).join(""),n=e.map((a,s)=>{let r=`diff-${s}`,d=a.note?`<p class="diff-note muted">${o(a.note)}</p>`:"";if(a.open)return Se(r,a,a.body(),d);return q.set(r,a.body),Se(r,a,"",d)}).join("");return`<div class="compare-tools">
      <ul class="file-index" aria-label="Changed files">${t}</ul>
      <div class="compare-buttons">
        <button type="button" class="btn btn-secondary btn-sm" data-action="expand-diffs">Expand all</button>
        <button type="button" class="btn btn-secondary btn-sm" data-action="collapse-diffs">Collapse all</button>
      </div>
    </div>
    <div class="diff-blocks">${n}</div>`}function Se(e,t,n,a){return`<details class="diff-file" id="${e}" data-diff-id="${e}"${t.open?" open":""}>
    <summary class="diff-summary">
      ${P(t.label,t.tone)}
      <code class="diff-name">${o(t.name)}</code>
      <span class="diff-counts"><span class="add-count">+${t.additions}</span> <span class="del-count">−${t.deletions}</span></span>
    </summary>
    ${a}
    <div class="diff-scroll" data-diff-body>${n}</div>
  </details>`}function F(e){return`<p class="diff-empty muted">${o(e)}</p>`}var xt={same:"",add:"row-add",del:"row-del"},Ct={same:'<span class="visually-hidden">unchanged</span> ',add:'<span class="visually-hidden">added: </span>+',del:'<span class="visually-hidden">removed: </span>−'};function Tt(e){return`<table class="diff-table"><caption class="visually-hidden">Difference between the two patches</caption><tbody>${Le(e,3).map((n)=>{if(n.kind==="gap")return`<tr class="row-gap"><td colspan="4">${n.hidden} unchanged ${n.hidden===1?"line":"lines"}</td></tr>`;return n.lines.map((a)=>`<tr class="${xt[a.op]}">
            <td class="gutter">${a.olderLine??""}</td>
            <td class="gutter">${a.newerLine??""}</td>
            <td class="marker">${Ct[a.op]}</td>
            <td class="code">${o(a.text)}</td>
          </tr>`).join("")}).join("")}</tbody></table>`}function U(e){if(e.patch===null)return F("Patch text is not available (binary or too large). Open it on GitHub.");let t=Ce(e.patch),n=t.slice(0,dt),a={hunk:"row-hunk",add:"row-add",del:"row-del",meta:"row-meta",context:""},s=n.map((d)=>`<tr class="${a[d.kind]}">
        <td class="gutter"></td>
        <td class="gutter">${d.line}</td>
        <td class="marker"></td>
        <td class="code">${o(d.text)}</td>
      </tr>`).join(""),r=t.length>n.length?`<tr class="row-gap"><td colspan="4">Showing the first ${n.length} of ${t.length} lines. Open the patch on GitHub for the rest.</td></tr>`:"";return`<table class="diff-table"><caption class="visually-hidden">Patch of ${o(e.filename)}</caption><tbody>${s}${r}</tbody></table>`}function Ie(e,t){if(!B)return;let n=c("cmp-result");if(n)n.innerHTML=Re(B,e,t);let a=c("cmp-cli");if(a)a.innerHTML=He(B,e,t)}function De(e){let t=e.querySelector("[data-diff-body]"),n=e.dataset.diffId??"",a=q.get(n);if(!t||!a||t.childElementCount>0)return;t.innerHTML=a(),q.delete(n)}function Z(e){document.querySelectorAll("#cmp-result details.diff-file").forEach((t)=>{t.open=e})}function je(){let e=c("base-ref")?.value.trim()||"suda/main",t=c("head-ref")?.value.trim()||"",n=c("diff-mode")?.value==="..."?"...":"..";return{base:e,head:t,op:n}}function G(e,t){let n=e.trim().replace(/^\/+|\/+$/g,"");if(t&&(n==="suda/main"||n==="refs/suda/main"||n==="main")&&t.mainSha)return t.mainSha;if(t&&(n==="suda/history"||n==="refs/suda/history"||n==="history")&&t.historySha)return t.historySha;let a=t?.workspaces.find((s)=>s.label===n||s.ref===n||s.ref===`refs/${n}`);return a?a.sha:n}function W(e){return e.base==="suda/main"&&e.op===".."?`/diff/${e.head}`:`/diff/${e.base}${e.op}${e.head}`}function St(e){let t=D(),n=t?.repo??"gosuda/1",a=e.head.length>0,s=a?`${window.location.origin}${W(e)}`:"",r=a?`https://github.com/${n}/compare/${G(e.base,t)}${e.op}${G(e.head,t)}`:"",d=a?`[Diff (${e.head})](${s})`:"",i=c("short-url-output"),u=c("github-url-output"),l=c("open-github-btn"),p=c("copy-short-btn"),h=c("copy-md-btn"),g=c("resolve-sha-btn");if(i)i.textContent=a?s:"Enter a head reference to build a link.";if(u)u.textContent=a?r:"The GitHub compare URL appears here.";if(l)l.href=a?W(e):"/diff",l.setAttribute("aria-disabled",a?"false":"true");for(let[b,C]of[[p,s],[h,d]])if(b)b.disabled=!a,b.setAttribute("data-copy",C);if(g)g.disabled=!a}async function Fe(){let e=je();if(!e.head)return"Enter a head reference first.";let t=await fetch(`/diff/${e.base}${e.op}${e.head}?format=json`,{headers:{accept:"application/json"},cache:"no-store"});if(!t.ok)throw Error(`HTTP ${t.status}`);let n=await t.json();if(!n.compareUrl)throw Error("no compare URL in the response");let a=c("github-url-output"),s=c("open-github-btn");if(a)a.textContent=n.compareUrl;if(s)s.href=n.compareUrl;return"Resolved to exact commits."}function M(){St(je())}function Be(e){let t=c("head-ref");if(t)t.value=e;M()}function Ne(e){let t=c("base-ref");if(t)t.value=e;M()}function Pt(e){if(!e)return'<p class="muted">Workspaces could not be loaded. Type a reference above instead.</p>';if(e.workspaces.length===0)return'<p class="muted">No active workspaces right now. Type a reference above, such as a commit sha.</p>';return`<ul class="pick-list">${e.workspaces.map((n)=>{let{ns:a}=X(n.label);return`<li class="pick-row">
        <span class="pick-ns">${o(a)}</span>
        <code class="pick-ref">${o(n.label)}</code>
        <span class="sha">${o(v(n.sha))}</span>
        <span class="pick-actions">
          <button type="button" class="btn btn-secondary btn-sm" data-action="pick-head" data-ref="${o(n.label)}">Use as head</button>
          <a class="btn btn-ghost btn-sm" href="/diff/${o(n.label)}" target="_blank" rel="noopener">Open diff<span class="visually-hidden"> in a new tab</span></a>
        </span>
      </li>`}).join("")}</ul>`}async function Oe(){let e=await A(),t=e.ok?e.value:null,a=se()??"",s={base:"suda/main",head:a,op:".."},r=a?`${window.location.origin}${W(s)}`:"",d=a?`https://github.com/${t?.repo??"gosuda/1"}/compare/${G("suda/main",t)}..${G(a,t)}`:"",i=a?`[Diff (${a})](${r})`:"";return`<header class="page-head">
    <div>
      <p class="eyebrow">Diff links</p>
      <h1 tabindex="-1">Diff links</h1>
      <p class="lead">Build a two-dot comparison on gosuda/1. Short links redirect through magpie to GitHub, so they work without a GitHub login. Names resolve when the link is followed; use <strong>Resolve exact SHAs</strong> to pin them.</p>
    </div>
  </header>
  ${e.ok?"":'<div class="banner banner-warn" role="status"><strong class="banner-title">Trunk state is unavailable.</strong> Names can still be typed; the links resolve when opened.</div>'}
  <section class="card" aria-labelledby="generator-title">
    <header class="card-head">
      <div>
        <h2 id="generator-title">Link generator</h2>
        <p class="card-lede">Base is what you compare from; head is what you compare to. <code>suda/main</code> is the trunk snapshot.</p>
      </div>
      <div class="preset-row" role="group" aria-label="Base presets">
        <button type="button" class="chip-button" data-action="set-base" data-ref="suda/main">Base: suda/main</button>
        <button type="button" class="chip-button" data-action="set-base" data-ref="suda/history">Base: suda/history</button>
      </div>
    </header>
    <form id="diff-form" class="form-grid form-grid-generator" novalidate>
      <div class="field">
        <label for="base-ref">Base</label>
        <p class="hint" id="base-hint">suda/main, suda/history, ws/actor/task, or a commit sha</p>
        <input id="base-ref" name="base" type="text" value="suda/main" aria-describedby="base-hint" autocomplete="off" spellcheck="false">
      </div>
      <div class="field">
        <label for="head-ref">Head</label>
        <p class="hint" id="head-hint">A workspace such as ws/actor/task, a land/ or review/ branch, or a sha</p>
        <input id="head-ref" name="head" type="text" value="${o(a)}" placeholder="ws/actor/task-id" aria-describedby="head-hint" autocomplete="off" spellcheck="false">
      </div>
      <div class="field">
        <label for="diff-mode">Comparison</label>
        <p class="hint" id="mode-hint">Two-dot is right for parentless trunk snapshots.</p>
        <select id="diff-mode" name="mode" aria-describedby="mode-hint">
          <option value="..">Two-dot (..): direct tree diff</option>
          <option value="...">Three-dot (...): from the merge base</option>
        </select>
      </div>
    </form>
    <div class="output-stack" aria-live="polite">
      <div class="output-row">
        <div class="output-meta">
          <span class="output-tag">Short link</span>
          <code id="short-url-output" class="output-code">${a?o(r):"Enter a head reference to build a link."}</code>
        </div>
        <div class="output-actions">
          <button type="button" id="copy-short-btn" class="btn btn-secondary" data-copy="${o(r)}"${a?"":" disabled"}>Copy short link</button>
          <button type="button" id="copy-md-btn" class="btn btn-ghost" data-copy="${o(i)}"${a?"":" disabled"}>Copy Markdown</button>
        </div>
      </div>
      <div class="output-row">
        <div class="output-meta">
          <span class="output-tag">GitHub compare</span>
          <code id="github-url-output" class="output-code">${a?o(d):"The GitHub compare URL appears here."}</code>
        </div>
        <div class="output-actions">
          <button type="button" id="resolve-sha-btn" class="btn btn-secondary" data-action="resolve"${a?"":" disabled"}>Resolve exact SHAs</button>
          <a id="open-github-btn" class="btn btn-primary" href="${a?o(W(s)):"/diff"}" target="_blank" rel="noopener">Open diff on GitHub</a>
        </div>
      </div>
      <p id="diff-status" class="output-status" role="status"></p>
    </div>
  </section>
  <section class="card" aria-labelledby="pick-title">
    <header class="card-head">
      <div>
        <h2 id="pick-title">Pick a head</h2>
        <p class="card-lede">Active workspaces on gosuda/1. Use one as the head, or open its diff against the trunk.</p>
      </div>
    </header>
    ${Pt(t)}
  </section>`}var Et="magpie.theme";function te(e){let t=e.replace(/\/+$/,"")||"/";if(t==="/"||t==="/index.html")return{view:"overview",param:""};if(t==="/changes")return{view:"changes",param:""};if(t.startsWith("/changes/")){let n="";try{n=decodeURIComponent(t.slice(9))}catch{n=t.slice(9)}return{view:"change",param:n}}if(t==="/diff")return{view:"diff",param:""};return{view:"notfound",param:""}}var Ht={overview:()=>ge(),changes:()=>be(),change:(e)=>Pe(e.param),diff:()=>Oe(),notfound:async()=>'<header class="page-head"><div><p class="eyebrow">Not found</p><h1 tabindex="-1">Page not found</h1><p class="lead">This page does not exist in Magpie. Start from the <a href="/">overview</a> or browse <a href="/changes">changes</a>.</p></div></header>'};function Rt(e){switch(e.view){case"overview":return"Overview · Magpie";case"changes":return"Changes · Magpie";case"change":if(/^[0-9]+$/.test(e.param))return`Review #${e.param} · Magpie`;return`${e.param.slice(0,9)||"Change"} · Magpie`;case"diff":return"Diff links · Magpie";case"notfound":return"Not found · Magpie"}}function ne(e,t){let n;if(e.view==="overview")n=t==="#workspaces"?"workspaces":t==="#projects"?"projects":"overview";else if(e.view==="change")n="changes";else n=e.view;document.querySelectorAll("[data-nav]").forEach((a)=>{if(a.dataset.nav===n)a.setAttribute("aria-current","page");else a.removeAttribute("aria-current")})}function ae(e){let t=e.slice(1);try{t=decodeURIComponent(t)}catch{}document.getElementById(t)?.scrollIntoView({block:"start"})}var Ue=0,qe=null;async function z(e){let t=++Ue,n=te(location.pathname);ne(n,location.hash);let a=c("view");if(!a)return;a.setAttribute("aria-busy","true");let s;try{s=await Ht[n.view](n)}catch(r){s=x("Something went wrong.",r instanceof Error?r.message:"Unknown error.")}if(t!==Ue)return;if(a.innerHTML=s,a.removeAttribute("aria-busy"),document.title=Rt(n),qe=location.pathname,location.hash)ae(location.hash);else if(e.scroll)window.scrollTo(0,0);if(e.focus)document.querySelector("#view h1")?.focus({preventScroll:!0})}function _e(e,t="push"){let n=new URL(e,location.href);Q(!1);let a=`${n.pathname}${n.search}${n.hash}`,s=`${location.pathname}${location.search}${location.hash}`;if(a!==s)if(t==="replace")history.replaceState(null,"",a);else history.pushState(null,"",a);if(n.pathname===location.pathname&&n.search===location.search&&n.hash){ne(te(n.pathname),n.hash),ae(n.hash);return}z({focus:!0,scroll:!0})}function Q(e){c("app")?.classList.toggle("nav-open",e),c("nav-toggle")?.setAttribute("aria-expanded",String(e))}function Ge(){let e=document.documentElement.getAttribute("data-theme");if(e==="light"||e==="dark")return e;return window.matchMedia("(prefers-color-scheme: dark)").matches?"dark":"light"}function ee(){let e=c("theme-toggle");if(!e)return;let t=Ge()==="dark"?"light":"dark";e.textContent=t==="dark"?"Dark":"Light",e.title=`Switch to ${t} theme`,e.setAttribute("aria-label",`Switch to ${t} theme`)}function At(){let e=Ge()==="dark"?"light":"dark";document.documentElement.setAttribute("data-theme",e);try{localStorage.setItem(Et,e)}catch{}ee()}function Mt(e){let t=c("trunk-main-link"),n=c("trunk-main-copy"),a=c("trunk-history-link"),s=c("trunk-history-copy"),r=e?.repo??"gosuda/1";if(t)t.textContent=e?.mainSha?v(e.mainSha):"unavailable",t.href=e?.mainSha?`https://github.com/${r}/tree/${e.mainSha}`:`https://github.com/${r}`;if(n)n.disabled=!e?.mainSha,n.setAttribute("data-copy",e?.mainSha??"");if(a)a.textContent=e?.historySha?v(e.historySha):"unavailable",a.href=e?.historySha?`https://github.com/${r}/commits/${e.historySha}`:`https://github.com/${r}`;if(s)s.disabled=!e?.historySha,s.setAttribute("data-copy",e?.historySha??"")}async function It(e){let t=e.getAttribute("data-copy")??"";if(!t)return;let n=await le(t),a=e.textContent;if(e.tagName==="BUTTON")e.textContent=n?"Copied":"Copy failed",window.setTimeout(()=>{e.textContent=a},1500);J(n?"Copied to clipboard.":"Could not copy. Select the text and copy it instead.")}function Dt(){return{actor:c("cli-actor")?.value??"",task:c("cli-task")?.value??"",paths:c("cli-paths")?.value??""}}async function jt(e,t){switch(e.dataset.action){case"reload":case"retry":t.preventDefault(),z({focus:!1,scroll:!1});return;case"use-head":t.preventDefault(),oe(e.dataset.ref??""),_e("/diff");return;case"pick-head":Be(e.dataset.ref??""),J(`Head set to ${e.dataset.ref??""}.`);return;case"set-base":Ne(e.dataset.ref??"suda/main");return;case"expand-diffs":Z(!0);return;case"collapse-diffs":Z(!1);return;case"resolve":{t.preventDefault();let a=c("diff-status");if(a)a.textContent="Resolving…";try{let s=await Fe();if(a)a.textContent=s}catch{if(M(),a)a.textContent="Could not resolve the names. The links still work; they resolve when opened."}return}}}function Ft(){document.addEventListener("click",(e)=>{if(e.defaultPrevented||e.button!==0)return;let t=e.target instanceof Element?e.target:null;if(!t)return;let n=t.closest("[data-copy]");if(n){e.preventDefault(),It(n);return}let a=t.closest("[data-action]");if(a){jt(a,e);return}let s=t.closest("a[href]");if(!s||s.target||s.hasAttribute("download"))return;if(e.metaKey||e.ctrlKey||e.shiftKey||e.altKey)return;let r=new URL(s.href);if(r.origin!==location.origin)return;e.preventDefault(),_e(r.href)}),document.addEventListener("input",(e)=>{let t=e.target instanceof HTMLInputElement?e.target:null;if(!t)return;if(t.dataset.filter==="changes")ve(t.value);else if(t.dataset.filter==="workspaces")pe(t.value);else if(t.dataset.field==="subtree")ue(t.value);else if(t.dataset.field==="cli")fe(Dt());else if(t.id==="base-ref"||t.id==="head-ref")M()}),document.addEventListener("change",(e)=>{let t=e.target instanceof HTMLSelectElement?e.target:null;if(!t)return;if(t.id==="diff-mode")M();else if(t.dataset.compare){let n=c("cmp-from")?.value??"base",a=c("cmp-to")?.value??"";Ie(n,a)}}),document.addEventListener("toggle",(e)=>{let t=e.target instanceof HTMLDetailsElement?e.target:null;if(t&&t.classList.contains("diff-file")&&t.open)De(t)},!0),window.addEventListener("popstate",()=>{if(Q(!1),location.pathname===qe){if(ne(te(location.pathname),location.hash),location.hash)ae(location.hash);return}z({focus:!1,scroll:!1})}),window.addEventListener("keydown",(e)=>{if(e.key==="Escape")Q(!1)})}function Bt(){c("theme-toggle")?.addEventListener("click",At),window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change",ee),ee()}function Nt(){c("nav-toggle")?.addEventListener("click",()=>{let e=c("app")?.classList.contains("nav-open")??!1;Q(!e)})}async function Ot(){Bt(),Nt(),Ft();let e=await A();Mt(e.ok?e.value:null),await z({focus:!1,scroll:!1})}if(typeof document<"u")Ot();export{te as resolveRoute};
