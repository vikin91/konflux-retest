# Konflux retry cases

Emit a comment when it can trigger the intended job on the requested PR or tag
and the job has a chance to pass. Check the latest attempt, the comment
trigger, and any required jobs or input images. A creation failure describes
one attempt. Use newer runs or live service evidence to assess current
availability.

## GitHub returns 404 while Konflux marks a check in progress

Retry a cataloged job when its latest applicable check from the Red Hat Konflux
app reports a PipelineRun start failure, the GitHub check-update request
returned `404 Not Found`, and GitHub associates the check with the open PR and
its exact current head. Verify at that head that the PipelineRun accepts
`/konflux-retest <job>`. Apply the normal milestone order and input checks.
Post the command in its own comment so a trigger that matches the entire comment
can fire.

On November 23, 2026, StackRox PR #24152 hit this error on
`central-db-on-push`, `retag-fact`, `retag-scanner-slim`, and
`operator-bundle-on-push`. All four had a matching PR comment trigger. The
bundle can run after its prerequisite jobs pass. A later
`central-db-on-push` run on the same cluster passed. [Failed attempt](https://github.com/stackrox/stackrox/pull/24152/checks?check_run_id=137718862171) · [Later successful run](https://github.com/stackrox/stackrox/pull/23854/checks?check_run_id=138001360319)

## Temporary admission policy blocks creation

Retry a cataloged PR job when Konflux reports that the
`deny-pipelinerun-create` admission policy temporarily disabled PipelineRun
creation and GitHub associates the check with exactly that open PR and its
current head. Apply the normal milestone order and input checks.
