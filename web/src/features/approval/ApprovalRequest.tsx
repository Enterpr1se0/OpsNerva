import { memo } from 'react'
import { useTranslation } from 'react-i18next'
import { FileText, ShieldAlert } from 'lucide-react'
import type { Approval, Host } from '../../types'
import { CopyablePre } from '../../components/CopyButton'
import { HighlightedCode } from '../../components/HighlightedCode'
import { inferScriptLanguage } from '../../lib/codeLanguage'
import { sshTunnelRoute } from '../../lib/utils'
import { CompactTable } from '../tools/components/CompactTable'
import { DiffViewer } from '../tools/components/DiffViewer'
import { fullProgram } from '../tools/request'
import { jsonRecord, numberValue, previewText, textValue, toolOutputPreviewChars, type JsonRecord } from '../tools/payload'

export const ApprovalRequest = memo(function ApprovalRequest({approval, request, hosts, pendingCount, rootExecution}: {approval: Approval; request: JsonRecord; hosts: Host[]; pendingCount: number; rootExecution: boolean}) {
  const { t } = useTranslation();
  const script = textValue(request.script);
  const program = fullProgram(request);
  const change = jsonRecord(request.change);
  const workspaceID = textValue(request.workspace_id);
  const filePath =
    textValue(request.remote_path) || textValue(request.relative_path);
  const requestMode = textValue(request.mode),
    relativePath = textValue(request.relative_path),
    remotePath = textValue(request.remote_path);
  const searchMatchMode = textValue(request.search_match_mode);
  const searchMatchModeLabel = searchMatchMode === "literal"
    ? t("tool.matchModeLiteral")
    : searchMatchMode === "regex"
      ? t("tool.matchModeRegex")
      : searchMatchMode || "—";
  const workspaceShellBackend = textValue(request.workspace_shell_backend);
  const workspaceShellApproval = requestMode === "workspace_shell_start";
  const hostWorkspaceShell =
    (requestMode === "workspace_shell" || workspaceShellApproval) && workspaceShellBackend === "host";
  const tunnelApproval = requestMode === "ssh_tunnel_start";
  const sshShellApproval = requestMode === "ssh_shell_start";
  const interactiveShellApproval = sshShellApproval || workspaceShellApproval;
  const fileReadApproval = [
    "remote_read",
    "remote_search",
    "workspace_read",
    "workspace_search",
  ].includes(requestMode);
  const fileSearchApproval = ["remote_search", "workspace_search"].includes(
    requestMode,
  );
  const workspaceUpload = requestMode === "workspace_upload";
  const workspaceDownload = requestMode === "workspace_download";
  const workspaceTransfer = workspaceUpload || workspaceDownload;
  const sshTransfer = requestMode === "ssh_file_transfer";
  const sourceHostID = textValue(request.source_host_id);
  const sourcePath = textValue(request.source_path);
  const elevated = request.elevated === true;
  const actionKind = script
    ? t("approval.actionScript")
    : t("approval.actionCommand");
  const approvalTitle = fileReadApproval
    ? rootExecution
      ? t(fileSearchApproval ? "approval.sudoSearchTitle" : "approval.sudoReadTitle")
      : t(fileSearchApproval ? "approval.searchTitle" : "approval.readTitle")
    : tunnelApproval
      ? t("approval.tunnelTitle")
    : interactiveShellApproval
      ? t("approval.sshShellTitle")
    : rootExecution
    ? filePath
      ? t("approval.sudoFileTitle")
      : t("approval.sudoTitle", { kind: actionKind })
    : tunnelApproval
    ? t("approval.tunnelLabel")
    : sshTransfer
      ? t("approval.transferTitle")
      : workspaceUpload
        ? t("approval.uploadTitle")
      : workspaceDownload
        ? t("approval.downloadTitle")
      : hostWorkspaceShell
        ? t("approval.hostShellTitle")
        : filePath
          ? t("approval.fileTitle")
          : t("approval.executeTitle", { kind: actionKind });
  const commandLabel = fileReadApproval
    ? rootExecution
      ? t(fileSearchApproval ? "approval.rootSearchLabel" : "approval.rootReadLabel")
      : t(fileSearchApproval ? "approval.searchLabel" : "approval.readLabel")
    : interactiveShellApproval
    ? t("approval.sshShellLabel")
    : sshTransfer
    ? t("approval.transferLabel")
    : workspaceUpload
      ? t("approval.uploadLabel")
    : workspaceDownload
      ? t("approval.downloadLabel")
    : rootExecution
      ? filePath
        ? t("approval.rootFileLabel")
        : t("approval.rootCommandLabel", { kind: actionKind })
      : filePath
        ? t("approval.fileLabel")
        : t("approval.commandLabel", { kind: actionKind });
  const target = hosts.find((host) => host.id === approval.host_id);
  const targetHost = target?.name || approval.host_id;
  const source = hosts.find((host) => host.id === sourceHostID);
  const sourceHost = source?.name || sourceHostID;
  const tunnelDirection = textValue(request.direction) || "local";
  const tunnelLocalHost = textValue(request.local_host) || "127.0.0.1";
  const tunnelLocalPort = numberValue(request.local_port);
  const tunnelRemoteHost = textValue(request.remote_host);
  const tunnelRemotePort = numberValue(request.remote_port);
  const tunnelOperation = sshTunnelRoute(targetHost,tunnelDirection,tunnelLocalHost,tunnelLocalPort,tunnelRemoteHost,tunnelRemotePort,t('tunnels.automaticPort'));
  const sshShellOperation = `${targetHost}:${textValue(request.cwd)||'~'} · PTY`;
	const workspaceShellOperation = `${workspaceID}:${textValue(request.cwd)||'.'} · PTY`;
  const operation = tunnelApproval
    ? tunnelOperation
    : interactiveShellApproval
    ? workspaceShellApproval ? workspaceShellOperation : sshShellOperation
    : sshTransfer
    ? `${sourceHost}:${sourcePath} → ${targetHost}:${remotePath}`
    : workspaceUpload
      ? `${workspaceID}:${relativePath} → ${targetHost}:${remotePath}`
    : workspaceDownload
      ? `${targetHost}:${remotePath} → ${workspaceID}:${relativePath}`
    : program ||
      script ||
      `${requestMode} ${filePath}${fileSearchApproval ? ` · ${searchMatchModeLabel} pattern=${JSON.stringify(textValue(request.search_pattern))}` : ""}`.trim() ||
      t("approval.pendingOperation");
  const targetHostIdentity = [targetHost, target?.id && target.id !== targetHost ? target.id : approval.host_id !== targetHost ? approval.host_id : ''].filter(Boolean).join(' · ')
  const sourceHostIdentity = [sourceHost, source?.id && source.id !== sourceHost ? source.id : sourceHostID !== sourceHost ? sourceHostID : ''].filter(Boolean).join(' · ')
  const hostName = workspaceUpload || sshTransfer
    ? targetHostIdentity
    : workspaceID
      ? `Workspace / ${workspaceID}`
      : targetHostIdentity;
  const executionIdentity = rootExecution ? "root" : "user";
  const expectedSHA = textValue(request.expected_sha256),
    expectedDestinationSHA = textValue(request.expected_destination_sha256),
    validator = textValue(request.validator);
  const fileApprovalParameters: Array<Array<unknown>> = fileReadApproval
    ? [
        ...(workspaceID
          ? [["workspace_id", workspaceID]]
          : [["host_id", approval.host_id]]),
        ["path", filePath],
        ...(fileSearchApproval
          ? [
              ["match_mode", searchMatchModeLabel],
              ["pattern", textValue(request.search_pattern)],
              ["context_lines", numberValue(request.context_lines)],
            ]
          : [
              ...(workspaceID
                ? []
                : [["metadata_only", request.metadata_only === true]]),
              ["max_bytes", numberValue(request.max_bytes)],
              ["offset_bytes", numberValue(request.offset_bytes)],
              ["tail_lines", numberValue(request.tail_lines)],
            ]),
        ...(workspaceID ? [] : [["elevated", elevated]]),
      ]
    : [];
  return <>
        <div className="approval-dialog-head">
          <div className="approval-dialog-icon">
            <ShieldAlert size={20} />
          </div>
          <div>
            <span>
              {t("approval.confirmation", {
                queue:
                  pendingCount > 1
                    ? t("approval.queue", { count: pendingCount })
                    : t("approval.currentSession"),
              })}
            </span>
            <h2 id="approval-dialog-title">{approvalTitle}</h2>
          </div>
        </div>
        <div className="approval-operation">
          <span className="approval-command-label">
            {commandLabel}
            {rootExecution && (
              <em>
                <ShieldAlert size={12} />
                root
              </em>
            )}
          </span>
          {rootExecution && (
            <div className="approval-root-warning">
              <ShieldAlert size={18} />
              <div>
                <b>{t("approval.rootWarning")}</b>
              </div>
            </div>
          )}
          {filePath && (
            <div className="approval-file-target">
              <FileText size={15} />
              <div>
                <b>
                  {workspaceUpload
                    ? `${workspaceID}:${relativePath} -> ${targetHost}:${remotePath}`
                    : workspaceDownload
                      ? `${targetHost}:${remotePath} -> ${workspaceID}:${relativePath}`
                    : sshTransfer
                      ? `${sourceHost}:${sourcePath} -> ${targetHost}:${remotePath}`
                      : filePath}
                </b>
                <span>
                  {change
                    ? `${t('tool.fileEdit')} · +${numberValue(change.additions)} / -${numberValue(change.deletions)}`
                    : sshTransfer && expectedSHA
                    ? `${t("approval.sourceSHA")} · ${expectedSHA}${expectedDestinationSHA ? ` · ${t("approval.destinationSHA")} · ${expectedDestinationSHA}` : ""}`
                    : (workspaceTransfer && expectedSHA)
                      ? `Expected SHA256 · ${expectedSHA}`
                      : ''}
                  {validator ? ` · Validator ${validator}` : ""}
                </span>
              </div>
            </div>
          )}
          {fileApprovalParameters.length > 0 && (
            <CompactTable
              title={t("tool.actualParameters")}
              columns={[t("tool.parameter"), t("tool.value")]}
              rows={fileApprovalParameters}
            />
          )}
          {change&&textValue(change.diff)?<DiffViewer change={change}/>:<CopyablePre value={script||(program&&operation===program?()=>fullProgram(request,true):operation)} preClassName="approval-command-preview"><HighlightedCode code={previewText(script || `${tunnelApproval||interactiveShellApproval?'':'$ '}${operation}`,toolOutputPreviewChars)} language={script?inferScriptLanguage(script):program?'bash':undefined} autoDetect/></CopyablePre>}
          <dl>
            <div>
              <dt>
                {workspaceUpload || sshTransfer
                  ? t("approval.targetHost")
                  : workspaceID
                    ? t("common.workspace")
                    : t("approval.targetHost")}
              </dt>
              <dd>{hostName}</dd>
            </div>
            {sshTransfer && (
              <div>
                <dt>{t("approval.sourceHost")}</dt>
                <dd>{sourceHostIdentity}</dd>
              </div>
            )}
            {workspaceDownload && (
              <div>
                <dt>{t("approval.sourceHost")}</dt>
                <dd>{targetHostIdentity}</dd>
              </div>
            )}
            <div>
              <dt>{t("approval.identity")}</dt>
              <dd>{executionIdentity}</dd>
            </div>
            {workspaceShellBackend && (
              <div>
                <dt>{t("approval.environment")}</dt>
                <dd>
                  {hostWorkspaceShell
                    ? t("approval.hostShell")
                    : t("tool.sandbox")}
                </dd>
              </div>
            )}
            <div>
              <dt>{t("approval.digest")}</dt>
              <dd>{approval.request_digest.slice(0, 12)}</dd>
            </div>
          </dl>
          {(hostWorkspaceShell || sshShellApproval) && (
            <div className="approval-host-shell-warning">
              <ShieldAlert size={14} />
              <span>{t(sshShellApproval?"approval.sshShellWarning":"approval.hostShellWarning")}</span>
            </div>
          )}
          {typeof request.reason === "string" && <p>{request.reason}</p>}
        </div>
  </>;
});
