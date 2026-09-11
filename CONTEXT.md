# Seer

Web reconnaissance tool: visit a given set of URLs once, keep a screenshot and the server's response for each, and browse the results to decide what deserves a closer look.

## Language

**Target**:
A single URL (scheme, host, port, path) that seer is asked to visit. Seer only visits Targets it is given; it never discovers them.
_Avoid_: Website, site, host, page

**Run**:
One invocation of seer over a set of Targets, yielding exactly one Capture per Target. A Run is completed once every Target has a Capture, or aborted if stopped before then; the Captures it already made survive either way.
_Avoid_: Scan, job, batch, session

**Capture**:
The record of one visit to one Target in one Run: succeeded (a Screenshot plus a Response) when the browser produced a document — any status code, even a blank page — or failed (the reason plus any partial Response) when it could not reach one at all. Captures of the same Target from different Runs coexist; a newer one never replaces an older one.
_Avoid_: Result, shot, snapshot, entry

**Screenshot**:
The image of the page as rendered during a Capture's visit.

**Response**:
What the Target's server returned during a Capture's visit: status code, final URL, redirect chain, every response header verbatim, page title, and a summary of the TLS certificate. Observed from the same visit that produced the Screenshot, never from a separate request.
_Avoid_: Server info, server header info, headers (for the whole record), metadata

**Report**:
The read-only view of a Run's Captures used to triage them. Viewing a Report never causes a visit.
_Avoid_: Dashboard, results page

**Store**:
The place Runs and their Captures are kept. One Store holds many Runs, typically one Store per engagement; a Report is always served out of a Store.
_Avoid_: Database, output directory, results
