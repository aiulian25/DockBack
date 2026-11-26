// useStepUp: run an action that the server may refuse until the password is
// confirmed. The first attempt goes out as it is. If the server answers "confirm
// it's you", a dialog asks for the password (and code), and the same action is
// tried again with them. Callers write the action once and render `dialog`.
import { useCallback, useState } from "react";
import { StepUpCredentials, StepUpError } from "../api";
import { Modal } from "./ui";
import StepUpPrompt from "./StepUpPrompt";

/** What an action that was not confirmed fails with, so callers can say nothing changed. */
export const STEP_UP_CANCELLED_MESSAGE = "not confirmed, so nothing was changed";

type Action<T> = (credentials?: StepUpCredentials) => Promise<T>;

interface Pending {
  totp: boolean;
  error: string;
  confirm: (credentials: StepUpCredentials) => Promise<void>;
  cancel: () => void;
}

export function useStepUp() {
  const [pending, setPending] = useState<Pending | null>(null);
  const [busy, setBusy] = useState(false);

  const confirmed = useCallback(async <T,>(action: Action<T>): Promise<T> => {
    try {
      return await action();
    } catch (error) {
      if (!(error instanceof StepUpError)) throw error;
      return askThenRetry(action, error.totp_required, setPending, setBusy);
    }
  }, []);

  const dialog = pending && (
    <Modal open onClose={pending.cancel} title="Confirm it's you" dismissible={!busy}>
      <StepUpPrompt totp={pending.totp} busy={busy} error={pending.error}
        onConfirm={(password, code) => { void pending.confirm({ password, code }); }} />
    </Modal>
  );

  return { confirmed, dialog };
}

// askThenRetry shows the prompt and settles once the action has gone through, has
// failed for another reason, or the prompt was closed. A wrong password keeps the
// prompt open with the server's message.
function askThenRetry<T>(
  action: Action<T>,
  totpRequired: boolean,
  setPending: (pending: Pending | null) => void,
  setBusy: (busy: boolean) => void,
): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    const ask = (totp: boolean, message: string) => setPending({
      totp,
      error: message,
      cancel: () => { setPending(null); reject(new Error(STEP_UP_CANCELLED_MESSAGE)); },
      confirm: async (credentials) => {
        setBusy(true);
        try {
          const result = await action(credentials);
          setPending(null);
          resolve(result);
        } catch (error) {
          if (error instanceof StepUpError) { ask(error.totp_required, error.message); return; }
          setPending(null);
          reject(error);
        } finally {
          setBusy(false);
        }
      },
    });
    ask(totpRequired, "");
  });
}
