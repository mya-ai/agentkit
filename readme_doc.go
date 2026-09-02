package agentkit

// referenceDoc is the developer reference whose file:line anchors and documented
// signatures are asserted against this package's source by readme_anchors_test.go
// and readme_identity_test.go.
//
// It is a constant rather than a literal in each test because the path is the one
// thing those guards share: if the doc moves and only one test is updated, the
// other reports "unreadable" and, historically, SKIPPED — a drift guard that stops
// guarding without telling anyone. The tests now fail instead.
const referenceDoc = "docs/reference.md"

// agentsDoc is the orientation file for AI coding agents. Its whole value is that an
// agent can cite an anchor from it WITHOUT opening the file, so its markdown line
// references get the same guard as the reference's — see TestAGENTSAnchorsResolve.
const agentsDoc = "AGENTS.md"

// docPaths are the documentation files the guards in this package read. They are
// listed here so a rename cannot leave a guard pointing at a path that no longer
// exists — see TestGuardedDocPathsExist.
var docPaths = []string{referenceDoc, agentsDoc}
