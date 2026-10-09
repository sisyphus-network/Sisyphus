import { LoaderCircle, Trash2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { ResponsiveActionDialog } from "@/components/ui/responsive-action-dialog";
import { useDialogValue } from "@/hooks/use-dialog-value";
type DeleteTarget = { id: string; title: string };

export function DeleteChatConfirmation({
  target,
  direction,
  deleteTitle,
  deleteDescription,
  cancelLabel,
  confirmLabel,
  deletingLabel,
  pending,
  onClose,
  onConfirm,
}: {
  target: DeleteTarget | null;
  direction: "ltr" | "rtl";
  deleteTitle: string;
  deleteDescription: string;
  cancelLabel: string;
  confirmLabel: string;
  deletingLabel: string;
  pending: boolean;
  onClose: () => void;
  onConfirm: () => void;
}) {
  const { displayedValue, onAfterClose } = useDialogValue(target);
  return <ResponsiveActionDialog open={Boolean(target)} onAfterClose={onAfterClose} direction={direction} title={deleteTitle} description={deleteDescription} icon={<Trash2 className="size-6 text-red-500" />} onOpenChange={(open) => { if (!open && !pending) onClose(); }}>
    {displayedValue ? <p className="break-words rounded-xl bg-[var(--app-wash)] px-3 py-2 text-sm font-medium text-[var(--app-ink)]">{displayedValue.title}</p> : null}
    <div className="mt-5 grid min-w-0 gap-2 sm:grid-cols-2">
      <Button type="button" variant="outline" size="lg" onClick={onClose} disabled={pending} className="order-2 h-12 min-w-0 w-full rounded-xl sm:order-1">{cancelLabel}</Button>
      <Button type="button" variant="destructive" size="lg" onClick={onConfirm} disabled={pending || !target} className="order-1 h-12 min-w-0 w-full whitespace-normal rounded-xl px-3 text-center leading-5 sm:order-2">{pending ? <LoaderCircle className="size-4 shrink-0 animate-spin" /> : <Trash2 className="size-4 shrink-0" />}{pending ? deletingLabel : confirmLabel}</Button>
    </div>
  </ResponsiveActionDialog>;
}
