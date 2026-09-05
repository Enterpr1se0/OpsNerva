import { useMemo, useState } from 'react'
import { createPortal } from 'react-dom'
import { useTranslation } from 'react-i18next'
import { Check, RefreshCw, ShieldAlert, Square, X } from 'lucide-react'
import type { Approval, Host } from '../../types'
import { CopyablePre } from '../../components/CopyButton'
import { HighlightedCode } from '../../components/HighlightedCode'
import { executionPermission } from '../tools/request'
import { previewStructuredValue, textValue, type JsonRecord } from '../tools/payload'
import { ApprovalRequest } from './ApprovalRequest'
import { CommandExplanationPanel } from './CommandExplanationPanel'
import { useApprovalActions, type ApprovalCallbacks } from './useApprovalActions'

export function ApprovalDialog({approval, pendingCount, hosts, running, stopping, onStop, dismissApproval, onApproved, onNotice}: ApprovalCallbacks & {
  approval: Approval;
  pendingCount: number;
  hosts: Host[];
  running: boolean;
  stopping: boolean;
  onStop: () => void;
}) {
  const { t } = useTranslation();
  const {note, setNote, decisionBusy, explanationBusy, error, decide, reject, retryExplanation} = useApprovalActions({approvalID: approval.id, dismissApproval, onApproved, onNotice});
  const [requestExpanded, setRequestExpanded] = useState(false);
  const requestJSON = approval.request_json;
  const request = useMemo<JsonRecord>(() => {
    try { return JSON.parse(requestJSON); }
    catch { return {request: requestJSON}; }
  }, [requestJSON]);
  const sourceHostIDs = textValue(request.mode) === 'ssh_file_transfer' ? [textValue(request.source_host_id)] : [];
  const rootExecution = executionPermission(request, hosts, approval.host_id, ...sourceHostIDs) === 'root';
  const explanationPending = approval.ai_review?.status === 'pending';
  const decisionDisabled = !!decisionBusy;
  return createPortal(
    <div className="approval-modal-backdrop">
      <section
        className={`approval-dialog ${rootExecution ? "elevated" : ""}`}
        role="dialog"
        aria-modal="true"
        aria-labelledby="approval-dialog-title"
      >
        <ApprovalRequest approval={approval} request={request} hosts={hosts} pendingCount={pendingCount} rootExecution={rootExecution}/>
        <CommandExplanationPanel review={approval.ai_review} />
        <div className="review-retry-row">
          <button
            disabled={decisionDisabled || explanationPending || explanationBusy}
            onClick={retryExplanation}
          >
            <RefreshCw
              className={explanationBusy || explanationPending ? "spin" : ""}
              size={13}
            />
            {explanationPending
              ? t("approval.reviewWorking")
              : explanationBusy
                ? t("approval.retrying")
                : t("approval.retryExplanation")}
          </button>
        </div>
        <label className="approval-guidance">
          <span>{t("approval.guidance")}</span>
          <textarea
            value={note}
            maxLength={2000}
            onChange={(event) => setNote(event.target.value)}
            autoFocus
          />
        </label>
        {error && (
          <div className="approval-dialog-error">
            <ShieldAlert size={14} />
            {error}
          </div>
        )}
        <details className="approval-request-detail" open={requestExpanded} onToggle={event=>setRequestExpanded(event.currentTarget.open)}>
          <summary>{t("approval.requestDetails")}</summary>
          {requestExpanded&&<CopyablePre value={()=>JSON.stringify(request,null,2)}><HighlightedCode code={JSON.stringify(previewStructuredValue(request),null,2)} language="json"/></CopyablePre>}
        </details>
        <div className="approval-choice-grid">
          <button
            className="allow-once"
            disabled={decisionDisabled || stopping}
            onClick={() => decide()}
          >
            <Check size={16} />
            <span>
              <b>
                {decisionBusy === "once"
                  ? t("approval.executing")
                  : rootExecution
                    ? t("approval.allowSudo")
                    : t("approval.allowOnce")}
              </b>
            </span>
          </button>
          <button
            className="reject-guidance"
            disabled={decisionDisabled || stopping || !note.trim()}
            onClick={reject}
          >
            <X size={16} />
            <span>
              <b>
                {decisionBusy === "reject"
                  ? t("approval.rejecting")
                  : t("approval.reject")}
              </b>
            </span>
          </button>
          <button
            className="stop-agent-run"
            disabled={decisionDisabled || stopping || !running}
            onClick={onStop}
          >
            <Square size={14} fill="currentColor" />
            <span>
              <b>
                {stopping ? t("approval.stopping") : t("approval.stopAgent")}
              </b>
            </span>
          </button>
        </div>
      </section>
    </div>,
    document.body,
  );
}
