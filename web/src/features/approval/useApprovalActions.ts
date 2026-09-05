import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { api } from '../../api/api'
import { errorText } from '../../lib/utils'
import type { ApprovalExecutionResult } from '../../types'

export type ApprovalCallbacks = {
  dismissApproval: (approvalID: string) => void;
  onApproved: (result: ApprovalExecutionResult) => void;
  onNotice: (message: string) => void;
};

export function useApprovalActions({approvalID, dismissApproval, onApproved, onNotice}: ApprovalCallbacks & {approvalID: string}) {
  const { t } = useTranslation();
  const [note, setNote] = useState("");
  const [decisionBusy, setDecisionBusy] = useState<"" | "once" | "reject">("");
  const [explanationBusy, setExplanationBusy] = useState(false);
  const [error, setError] = useState("");
  const decide = async () => {
    setDecisionBusy("once");
    setError("");
    try {
      const result = await api.approve(
        approvalID,
        note.trim() || "Reviewed and approved.",
      );
      onApproved(result);
      onNotice(
        t("approval.approved", {
          status: t(`statusLabels.${result.status}`, {
            defaultValue: result.status,
          }),
          run: result.run_id,
        }),
      );
      dismissApproval(approvalID);
    } catch (err) {
      setError(errorText(err));
    } finally {
      setDecisionBusy("");
    }
  };
  const reject = async () => {
    const instruction = note.trim();
    if (!instruction) {
      setError(t("approval.replacementRequired"));
      return;
    }
    setDecisionBusy("reject");
    setError("");
    try {
      await api.reject(approvalID, instruction);
      onNotice(t("approval.rejected"));
      dismissApproval(approvalID);
    } catch (err) {
      setError(errorText(err));
    } finally {
      setDecisionBusy("");
    }
  };
  const retryExplanation = async () => {
    setExplanationBusy(true);
    setError("");
    try {
      const updated = await api.retryApprovalExplanation(approvalID);
      const status = updated.ai_review?.status;
      onNotice(
        status === "completed"
          ? t("approval.explanationReady")
          : t("approval.explanationDegraded"),
      );
    } catch (err) {
      setError(errorText(err));
    } finally {
      setExplanationBusy(false);
    }
  };
  return {note, setNote, decisionBusy, explanationBusy, error, decide, reject, retryExplanation};
}
