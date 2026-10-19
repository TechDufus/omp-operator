/*
Copyright 2026 TechDufus.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package dashboard

import (
	"html/template"
	"strings"
)

const layoutTmpl = `{{define "layout"}}<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
{{if .Refresh}}<meta http-equiv="refresh" content="10">
{{end}}<title>{{.Title}} - omp-operator</title>
<style>
:root{color-scheme:light dark;--bg:#fafafa;--fg:#1c1c1e;--muted:#6b6b72;--line:#d8d8de;--card:#fff;--link:#0b5cd5;--ok:#1a7f37;--warn:#9a6700;--bad:#c62828}
@media (prefers-color-scheme:dark){:root{--bg:#121214;--fg:#e7e7ea;--muted:#9a9aa3;--line:#2e2e35;--card:#1b1b1f;--link:#6aa6ff;--ok:#56d364;--warn:#e3b341;--bad:#ff7b72}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}
header{display:flex;flex-wrap:wrap;gap:.5rem 1rem;align-items:center;justify-content:space-between;padding:.6rem 1rem;border-bottom:1px solid var(--line);background:var(--card)}
header a.brand{font-weight:700;color:var(--fg);text-decoration:none}
header .who{color:var(--muted);font-size:.9em}
main{max-width:72rem;margin:0 auto;padding:1rem}
h1{font-size:1.4rem;margin:.2rem 0 1rem;overflow-wrap:anywhere}
h2{font-size:1.1rem;margin:1.6rem 0 .5rem}
a{color:var(--link)}
.muted{color:var(--muted)}
.scroll{overflow-x:auto}
table{border-collapse:collapse;width:100%;background:var(--card);border:1px solid var(--line)}
th,td{padding:.35rem .6rem;border-bottom:1px solid var(--line);text-align:left;vertical-align:top}
th{font-size:.8em;text-transform:uppercase;letter-spacing:.04em;color:var(--muted);white-space:nowrap}
dl{display:grid;grid-template-columns:max-content 1fr;gap:.25rem 1rem;margin:0;background:var(--card);border:1px solid var(--line);padding:.6rem .8rem}
dt{color:var(--muted)}
dd{margin:0;overflow-wrap:anywhere}
pre{background:var(--card);border:1px solid var(--line);padding:.6rem .8rem;margin:0;white-space:pre-wrap;overflow-wrap:anywhere;font:13px/1.45 ui-monospace,SFMono-Regular,Menlo,monospace}
code{font:13px ui-monospace,SFMono-Regular,Menlo,monospace;overflow-wrap:anywhere}
.b{display:inline-block;padding:0 .45rem;border-radius:.7rem;border:1px solid var(--line);font-size:.85em;white-space:nowrap}
.True,.ok,.fixed,.Succeeded,.done{color:var(--ok)}
.needs_owner,.inconclusive,.Pending,.Running,.Unknown,.Working,.Waiting,.queued,.running{color:var(--warn)}
.False,.failed,.Failed,.cancelled{color:var(--bad)}
ul{margin:.2rem 0;padding-left:1.2rem}
.ask{border:1px solid var(--warn);border-left-width:.35rem;background:var(--card);padding:.6rem .8rem}
tr.pending td{background:color-mix(in srgb,var(--warn) 12%,var(--card))}
@media (max-width:40rem){dl{grid-template-columns:1fr}dt{margin-top:.4rem}}
</style>
</head>
<body>
<header>
<a class="brand" href="/">omp-operator</a>
<span class="who">{{if .User}}{{.User}}{{end}}{{if .Session}} &middot; <a href="/auth/logout">sign out</a>{{end}}</span>
</header>
<main>
<h1>{{.Title}}</h1>
{{template "body" .Body}}
</main>
</body>
</html>
{{end}}
{{define "sessions"}}{{if .}}<div class="scroll"><table>
<thead><tr><th>Session</th><th>Agent</th><th>Trigger</th><th>Kind</th><th>Phase</th><th>Verdict</th><th>Title</th><th>Age</th></tr></thead>
<tbody>
{{range .}}<tr><td><a href="{{.Link}}">{{if .Namespace}}{{.Namespace}}/{{end}}{{.Name}}</a></td><td>{{.Agent}}</td><td>{{.Trigger}}</td><td>{{.Kind}}</td><td><span class="{{.Phase}}">{{.Phase}}</span></td><td><span class="{{.Verdict}}">{{.Verdict}}</span></td><td>{{.Title}}</td><td title="{{.Created}}">{{.Age}}</td></tr>
{{end}}</tbody></table></div>{{else}}<p class="muted">No sessions.</p>{{end}}{{end}}
{{define "threads"}}{{if .}}<div class="scroll"><table>
<thead><tr><th>Thread</th><th>Agent</th><th>Phase</th><th>Turns</th><th>Latest</th><th>Title</th><th>Age</th></tr></thead>
<tbody>
{{range .}}<tr><td><a href="{{.Link}}">{{.Namespace}}/{{.Name}}</a></td><td>{{.Agent}}</td><td><span class="{{.Phase}}">{{.Phase}}</span></td><td>{{.Turns}}</td><td><span class="{{.Latest}}">{{.Latest}}</span></td><td>{{.Title}}</td><td title="{{.Created}}">{{.Age}}</td></tr>
{{end}}</tbody></table></div>{{else}}<p class="muted">No threads.</p>{{end}}{{end}}
{{define "triggers"}}{{if .}}<div class="scroll"><table>
<thead><tr><th>Trigger</th><th>Agent</th><th>Type</th><th>Enabled</th><th>Ready</th></tr></thead>
<tbody>
{{range .}}<tr><td><a href="{{.Link}}">{{.Namespace}}/{{.Name}}</a></td><td>{{.Agent}}</td><td>{{.Type}}</td><td>{{if .Enabled}}yes{{else}}no{{end}}</td><td><span class="{{.Ready}}">{{.Ready}}</span>{{if .ReadyReason}} <span class="muted">{{.ReadyReason}}</span>{{end}}</td></tr>
{{end}}</tbody></table></div>{{else}}<p class="muted">No triggers.</p>{{end}}{{end}}
{{define "counts"}}ok {{.OK}} &middot; fixed {{.Fixed}} &middot; needs owner {{.NeedsOwner}} &middot; inconclusive {{.Inconclusive}} &middot; failed {{.Failed}} &middot; active {{.Active}}{{end}}
{{define "budget"}}events {{.EventUsed}}/{{.EventLimit}} &middot; non-critical {{.NonCriticalUsed}}/{{.NonCritLim}}{{end}}
{{define "pairs"}}{{if .}}<dl>{{range .}}<dt>{{index . 0}}</dt><dd>{{index . 1}}</dd>{{end}}</dl>{{else}}<p class="muted">-</p>{{end}}{{end}}
{{define "list"}}{{if .}}<ul>{{range .}}<li>{{.}}</li>{{end}}</ul>{{else}}<span class="muted">none</span>{{end}}{{end}}
`

const indexTmpl = `{{define "body"}}
{{if .NoAccess}}<p class="muted">No namespaces are granted to your account.</p>{{end}}
<h2>Agents</h2>
{{if .Agents}}<div class="scroll"><table>
<thead><tr><th>Agent</th><th>Ready</th><th>Model</th><th>Sessions, 24 h</th><th>Budgets, 24 h</th></tr></thead>
<tbody>
{{range .Agents}}<tr><td><a href="{{.Link}}">{{.Namespace}}/{{.Name}}</a></td><td><span class="{{.Ready}}">{{.Ready}}</span>{{if .ReadyReason}} <span class="muted">{{.ReadyReason}}</span>{{end}}</td><td><code>{{.Model}}</code></td><td>{{template "counts" .Counts}}</td><td>{{template "budget" .Budget}}</td></tr>
{{end}}</tbody></table></div>{{else}}<p class="muted">No agents.</p>{{end}}
<h2>Triggers</h2>
{{template "triggers" .Triggers}}
<h2>Threads</h2>
{{template "threads" .Threads}}
<h2>Newest sessions</h2>
{{template "sessions" .Sessions}}
{{end}}`

const agentTmpl = `{{define "body"}}
<dl>
<dt>Agent</dt><dd>{{.Namespace}}/{{.Name}}</dd>
<dt>Ready</dt><dd><span class="{{.Ready}}">{{.Ready}}</span>{{if .ReadyReason}} {{.ReadyReason}}{{end}}{{if .ReadyMessage}}: {{.ReadyMessage}}{{end}}</dd>
<dt>Model</dt><dd><code>{{.Model}}</code>{{if .Small}} (small: <code>{{.Small}}</code>){{end}}, thinking {{.Thinking}}</dd>
{{if .Gateway}}<dt>Gateway</dt><dd>{{.Gateway}}</dd>{{end}}
<dt>Image</dt><dd><code>{{.Image}}</code></dd>
{{if .ServiceAccount}}<dt>Service account</dt><dd>{{.ServiceAccount}}</dd>{{end}}
<dt>Tools</dt><dd>{{.Tools}}</dd>
{{if .Toolchain}}<dt>Toolchain</dt><dd>{{.Toolchain}}</dd>{{end}}
<dt>Limits</dt><dd>{{range $i, $l := .Limits}}{{if $i}} &middot; {{end}}{{$l}}{{end}}</dd>
<dt>Notify</dt><dd>{{template "list" .Notify}}</dd>
<dt>Sessions, 24 h</dt><dd>{{template "counts" .Counts}}</dd>
<dt>Budgets, 24 h</dt><dd>{{template "budget" .Budget}}</dd>
</dl>
<h2>Skill sources</h2>
{{template "list" .Skills}}
<h2>Workspace</h2>
{{template "list" .Workspace}}
<h2>Credentials</h2>
{{if .Credentials}}<div class="scroll"><table>
<thead><tr><th>Name</th><th>Description</th><th>Where</th></tr></thead>
<tbody>
{{range .Credentials}}<tr><td>{{.Name}}</td><td>{{.Description}}</td><td>{{.Where}}</td></tr>
{{end}}</tbody></table></div>{{else}}<p class="muted">none</p>{{end}}
<h2>MCP servers</h2>
{{template "list" .MCP}}
<h2>Actions</h2>
{{template "list" .Actions}}
<h2>Triggers</h2>
{{template "triggers" .Triggers}}
<h2>Newest sessions</h2>
{{template "sessions" .Sessions}}
{{end}}`

const triggerTmpl = `{{define "body"}}
<dl>
<dt>Trigger</dt><dd>{{.Namespace}}/{{.Name}}</dd>
<dt>Agent</dt><dd><a href="{{.AgentLink}}">{{.Agent}}</a></dd>
<dt>Type</dt><dd>{{.Type}}</dd>
<dt>Enabled</dt><dd>{{if .Enabled}}yes{{else}}no{{end}}</dd>
<dt>Ready</dt><dd><span class="{{.Ready}}">{{.Ready}}</span>{{if .ReadyReason}} {{.ReadyReason}}{{end}}{{if .ReadyMessage}}: {{.ReadyMessage}}{{end}}</dd>
<dt>Priority</dt><dd>{{.Priority}}</dd>
<dt>Resume</dt><dd>{{.Resume}}</dd>
{{if .HookURL}}<dt>Hook URL</dt><dd><code>{{.HookURL}}</code></dd>
<dt>Adapter</dt><dd>{{.Adapter}}</dd>
<dt>Auth</dt><dd>{{.Auth}}</dd>
{{if .AlertFilter}}<dt>Alert filter</dt><dd><code>{{.AlertFilter}}</code></dd>{{end}}
<dt>Event filter</dt><dd><code>{{.Filter}}</code></dd>{{end}}
</dl>
{{if .Mapping}}<h2>Mapping</h2>{{template "pairs" .Mapping}}{{end}}
{{if .Grouping}}<h2>Grouping</h2>{{template "pairs" .Grouping}}{{end}}
{{if .Schedule}}<h2>Schedule</h2>{{template "pairs" .Schedule}}{{end}}
{{if .HookURL}}<h2>Groups</h2>
{{if .Groups}}<div class="scroll"><table>
<thead><tr><th>Group</th><th>Name</th><th>Severity</th><th>Episode start</th><th>Last notified</th><th>Resolved</th><th>Triaged</th><th>Last session</th></tr></thead>
<tbody>
{{range .Groups}}<tr><td><code>{{.ID}}</code></td><td>{{.Name}}</td><td>{{.Severity}}</td><td>{{.EpisodeStart}}</td><td>{{.LastNotified}}</td><td>{{.Resolved}}</td><td>{{.Triaged}}</td><td>{{if .LastSessionLink}}<a href="{{.LastSessionLink}}">{{.LastSession}}</a>{{else}}-{{end}}</td></tr>
{{end}}</tbody></table></div>{{else}}<p class="muted">No groups.</p>{{end}}{{end}}
<h2>Newest sessions</h2>
{{template "sessions" .Sessions}}
{{end}}`

const sessionTmpl = `{{define "body"}}
<dl>
<dt>Session</dt><dd>{{.Namespace}}/{{.Name}}</dd>
<dt>Agent</dt><dd><a href="{{.AgentLink}}">{{.Agent}}</a></dd>
{{if .Trigger}}<dt>Trigger</dt><dd><a href="{{.TriggerLink}}">{{.Trigger}}</a></dd>{{end}}
<dt>Kind</dt><dd>{{.Kind}}</dd>
{{if .Event}}<dt>Event</dt><dd>{{.Event}}</dd>{{end}}
{{if .ThreadLink}}<dt>Thread</dt><dd><a href="{{.ThreadLink}}">{{.Thread}}</a></dd>{{end}}
<dt>Phase</dt><dd><span class="{{.Phase}}">{{.Phase}}</span>{{if .Outcome}} <span class="muted">outcome {{.Outcome}}</span>{{end}}</dd>
<dt>Verdict</dt><dd><span class="{{.Verdict}}">{{if .Verdict}}{{.Verdict}}{{else}}-{{end}}</span></dd>
<dt>Title</dt><dd>{{if .Title}}{{.Title}}{{else}}-{{end}}</dd>
<dt>Attempts</dt><dd>{{.Attempts}}{{if .Pod}} (pod <code>{{.Pod}}</code>){{end}}</dd>
<dt>Created</dt><dd>{{.Created}}</dd>
<dt>Started</dt><dd>{{.Started}}</dd>
<dt>Completed</dt><dd>{{.Completed}}</dd>
{{if .Revision}}<dt>Workspace</dt><dd><code>{{.Revision}}</code></dd>{{end}}
{{if .Usage}}<dt>Usage</dt><dd>{{.Usage}}</dd>{{end}}
{{if .Continuity}}<dt>Continuity</dt><dd>{{.Continuity}}</dd>{{end}}
{{if .TranscriptLink}}<dt>Transcript</dt><dd><a href="{{.TranscriptLink}}">open transcript</a></dd>{{end}}
</dl>
{{if .Error}}<h2>Error</h2><pre>{{.Error}}</pre>{{end}}
{{if .Summary}}<h2>Summary</h2><pre>{{.Summary}}</pre>{{end}}
{{if .ProposedCommands}}<h2>Proposed commands</h2><pre>{{range .ProposedCommands}}{{.}}
{{end}}</pre>{{end}}
{{if .Actions}}<h2>Actions</h2><div class="scroll"><table>
<thead><tr><th>Action</th><th>Target</th><th>OK</th><th>At</th><th>Result</th></tr></thead>
<tbody>
{{range .Actions}}<tr><td>{{.Action}}</td><td>{{.Target}}</td><td>{{if .OK}}yes{{else}}no{{end}}</td><td>{{.At}}</td><td>{{.Result}}</td></tr>
{{end}}</tbody></table></div>{{end}}
{{if .Report}}<h2>Report</h2><pre>{{.Report}}</pre>{{end}}
{{if .Events}}<h2>Events</h2><div class="scroll"><table>
<thead><tr><th>At</th><th>Kind</th><th>Text</th></tr></thead>
<tbody>
{{range .Events}}<tr><td>{{.At}}</td><td>{{.Kind}}</td><td>{{.Text}}</td></tr>
{{end}}</tbody></table></div>{{end}}
{{end}}`

const threadTmpl = `{{define "body"}}
<dl>
<dt>Thread</dt><dd>{{.Namespace}}/{{.Name}}</dd>
<dt>Title</dt><dd>{{if .Title}}{{.Title}}{{else}}-{{end}}</dd>
<dt>Agent</dt><dd><a href="{{.AgentLink}}">{{.Agent}}</a></dd>
<dt>Owner</dt><dd>{{.Owner}}</dd>
<dt>Phase</dt><dd><span class="{{.Phase}}">{{.Phase}}</span>{{if .Queued}} <span class="muted">{{.Queued}} queued</span>{{end}}</dd>
<dt>Model</dt><dd>{{if .Model}}<code>{{.Model}}</code>{{else}}agent default{{end}}{{if .Thinking}}, thinking {{.Thinking}}{{end}}</dd>
<dt>Repository</dt><dd>{{.Repo}}</dd>
{{if .Base}}<dt>Base</dt><dd><code>{{.Base}}</code></dd>{{end}}
{{if .LocalBranch}}<dt>Local branch</dt><dd>{{.LocalBranch}}</dd>{{end}}
<dt>Result branch</dt><dd><code>{{.Branch}}</code></dd>
{{if .Paths}}<dt>Paths</dt><dd>{{.Paths}}</dd>{{end}}
{{if .SecretFiles}}<dt>Secret files</dt><dd>{{template "list" .SecretFiles}}</dd>{{end}}
{{if .SecretEnv}}<dt>Secret env</dt><dd>{{template "list" .SecretEnv}}</dd>{{end}}
<dt>Turns</dt><dd>{{.TurnCount}}</dd>
<dt>Created</dt><dd>{{.Created}}</dd>
<dt>Last activity</dt><dd>{{.LastActivity}}</dd>
{{if .ActiveSession}}<dt>Active sitting</dt><dd><a href="{{.ActiveLink}}">{{.ActiveSession}}</a></dd>{{end}}
{{if .Conditions}}<dt>Conditions</dt><dd>{{template "list" .Conditions}}</dd>{{end}}
</dl>
{{if .Pending}}<h2>Waiting for your answer</h2>
<div class="ask">
<p><strong>Turn {{.Pending.Turn}}, question {{.Pending.ID}}</strong>{{if .Pending.Header}} &middot; {{.Pending.Header}}{{end}}{{if gt .Pending.Remaining 1}} <span class="muted">({{.Pending.Remaining}} questions in this ask are open)</span>{{end}}</p>
<pre>{{.Pending.Text}}</pre>
{{if .Pending.Options}}<ul>{{range .Pending.Options}}<li><strong>{{.Label}}</strong>{{if .Recommended}} <span class="muted">(recommended)</span>{{end}}{{if .Description}} <span class="muted">&mdash; {{.Description}}</span>{{end}}</li>{{end}}</ul>{{end}}
<p class="muted">Answer with <code>{{.AnswerHint}}</code>. Without an answer the agent is told to continue with its best judgment.</p>
</div>
{{end}}
<h2>Result</h2>
{{if .Result}}<dl>
<dt>Head</dt><dd><code>{{.Result.Head}}</code></dd>
<dt>Changes</dt><dd>{{.Result.Changes}} in {{.Result.Files}} files</dd>
<dt>Push</dt><dd>{{.Result.Pushed}}</dd>
<dt>Pull it</dt><dd><code>{{.PullHint}}</code></dd>
</dl>{{else}}<p class="muted">No result yet.</p>{{end}}
<h2>Turns</h2>
{{if .Turns}}<div class="scroll"><table>
<thead><tr><th>Turn</th><th>State</th><th>Message</th><th>Title</th><th>Reply</th><th>Error</th><th>Usage</th></tr></thead>
<tbody>
{{range .Turns}}<tr><td>{{.ID}}</td><td><span class="{{.State}}">{{.State}}</span></td><td><pre>{{.Text}}</pre></td><td>{{.Title}}</td><td>{{if .Reply}}<pre>{{.Reply}}</pre>{{end}}</td><td>{{.Error}}</td><td>{{.Usage}}{{if .Head}}<br><code>{{.Head}}</code>{{end}}{{if .Push}}<br>{{.Push}}{{end}}</td></tr>
{{end}}</tbody></table></div>{{else}}<p class="muted">No turns.</p>{{end}}
{{if .Questions}}<h2>Questions</h2>
<div class="scroll"><table>
<thead><tr><th>Turn</th><th>#</th><th>Question</th><th>Status</th><th>Answer</th></tr></thead>
<tbody>
{{range .Questions}}<tr{{if .Pending}} class="pending"{{end}}><td>{{.Turn}}</td><td>{{.ID}}</td><td>{{if .Header}}<strong>{{.Header}}</strong><br>{{end}}<pre>{{.Text}}</pre>{{if .Options}}<ul>{{range .Options}}<li>{{.Label}}{{if .Recommended}} <span class="muted">(recommended)</span>{{end}}{{if .Description}} <span class="muted">&mdash; {{.Description}}</span>{{end}}</li>{{end}}</ul>{{end}}</td><td><span class="{{if .Waiting}}Waiting{{else if .Expired}}cancelled{{else}}done{{end}}">{{.Status}}</span><br><span class="muted" title="{{.Asked}}">asked {{.Asked}}</span></td><td>{{if .Selected}}<strong>{{.Selected}}</strong>{{end}}{{if .Free}}<pre>{{.Free}}</pre>{{end}}{{if .By}}<span class="muted">by {{.By}}{{if .Answered}} at {{.Answered}}{{end}}</span>{{else if .Answered}}<span class="muted">at {{.Answered}}</span>{{end}}</td></tr>
{{end}}</tbody></table></div>
{{end}}
{{if .EventsSession}}<h2>Events</h2>
<p class="muted">Newest sitting: <a href="{{.EventsLink}}">{{.EventsSession}}</a></p>
{{if .Events}}<div class="scroll"><table>
<thead><tr><th>At</th><th>Kind</th><th>Text</th></tr></thead>
<tbody>
{{range .Events}}<tr><td>{{.At}}</td><td>{{.Kind}}</td><td>{{.Text}}</td></tr>
{{end}}</tbody></table></div>{{else}}<p class="muted">No events.</p>{{end}}{{end}}
{{end}}`

const signedOutTmpl = `{{define "body"}}<p>You are signed out.</p><p><a href="/auth/login">Sign in again</a></p>{{end}}`

// templates holds one parsed template set per page, each defining "layout" and its own "body".
var templates = func() map[string]*template.Template {
	pages := map[string]string{
		"index":     indexTmpl,
		"agent":     agentTmpl,
		"trigger":   triggerTmpl,
		"session":   sessionTmpl,
		"thread":    threadTmpl,
		"signedout": signedOutTmpl,
	}
	out := make(map[string]*template.Template, len(pages))
	for name, body := range pages {
		out[name] = template.Must(template.New(name).Parse(strings.TrimSpace(layoutTmpl) + "\n" + body))
	}
	return out
}()
