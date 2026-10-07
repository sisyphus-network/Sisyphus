"use client";

import { CircleAlert, CircleCheck, LoaderCircle } from "lucide-react";
import { forwardRef, useId, useImperativeHandle, useRef, useState, type InputHTMLAttributes, type ReactNode } from "react";
import { AnimatePresence, motion } from "motion/react";
import { cn } from "@/lib/utils";

type ValidationMessages = {
  valueMissing?: string;
  typeMismatch?: string;
  patternMismatch?: string;
  custom?: string;
};

type FloatingLabelInputProps = Omit<InputHTMLAttributes<HTMLInputElement>, "size"> & {
  label: string;
  icon?: ReactNode;
  helperText?: string;
  errorMessage?: string;
  validationMessages?: ValidationMessages;
  loading?: boolean;
  success?: boolean;
  validate?: (value: string) => string | undefined;
  containerClassName?: string;
};

export const FloatingLabelInput = forwardRef<HTMLInputElement, FloatingLabelInputProps>(function FloatingLabelInput(
  {
    id,
    label,
    icon,
    helperText,
    errorMessage,
    validationMessages,
    loading = false,
    success = false,
    validate,
    containerClassName,
    className,
    value,
    defaultValue,
    disabled,
    required,
    type = "text",
    placeholder,
    onBlur,
    onChange,
    onFocus,
    onInvalid,
    ...props
  },
  ref,
) {
  const generatedId = useId();
  const inputId = id ?? generatedId;
  const inputRef = useRef<HTMLInputElement>(null);
  const [focused, setFocused] = useState(false);
  const [touched, setTouched] = useState(false);
  const [uncontrolledValue, setUncontrolledValue] = useState(String(defaultValue ?? ""));
  const [nativeError, setNativeError] = useState<string>();

  useImperativeHandle(ref, () => inputRef.current as HTMLInputElement);

  const currentValue = value === undefined ? uncontrolledValue : String(value ?? "");
  const messageId = `${inputId}-message`;
  const resolveError = (input: HTMLInputElement) => {
    const customError = validate?.(input.value);
    if (customError) return customError;
    if (input.validity.valid) return undefined;
    if (input.validity.valueMissing) return validationMessages?.valueMissing ?? input.validationMessage;
    if (input.validity.typeMismatch) return validationMessages?.typeMismatch ?? input.validationMessage;
    if (input.validity.patternMismatch) return validationMessages?.patternMismatch ?? input.validationMessage;
    return validationMessages?.custom ?? input.validationMessage;
  };
  const validateInput = (input: HTMLInputElement) => setNativeError(resolveError(input));
  const resolvedError = errorMessage ?? nativeError;
  const hasError = Boolean(resolvedError);
  const hasValue = currentValue.trim().length > 0;
  const isFloating = focused || hasValue;
  const showSuccess = !hasError && !loading && (success || (touched && hasValue && Boolean(inputRef.current?.validity.valid)));
  const isDisabled = disabled || loading;

  return <div className={cn("group/floating-input", containerClassName)}>
    <div className="relative">
      {icon && <span aria-hidden="true" className={cn("pointer-events-none absolute start-3.5 top-1/2 z-10 -translate-y-1/2 text-[var(--app-muted)] transition-colors", focused && "text-[var(--app-ink)]", isDisabled && "opacity-55")}>{icon}</span>}
      <input
        {...props}
        ref={inputRef}
        id={inputId}
        type={type}
        value={value}
        defaultValue={value === undefined ? defaultValue : undefined}
        required={required}
        disabled={isDisabled}
        placeholder={isFloating ? placeholder : " "}
        aria-invalid={hasError || undefined}
        aria-describedby={helperText || resolvedError ? messageId : undefined}
        onFocus={(event) => {
          setFocused(true);
          onFocus?.(event);
        }}
        onBlur={(event) => {
          setFocused(false);
          setTouched(true);
          validateInput(event.currentTarget);
          onBlur?.(event);
        }}
        onChange={(event) => {
          if (value === undefined) setUncontrolledValue(event.target.value);
          if (touched) validateInput(event.currentTarget);
          onChange?.(event);
        }}
        onInvalid={(event) => {
          event.preventDefault();
          setTouched(true);
          validateInput(event.currentTarget);
          onInvalid?.(event);
        }}
        className={cn(
          "h-14 w-full rounded-2xl border bg-[var(--app-card)] pe-12 text-sm text-[var(--app-ink)] outline-none transition-[border-color,box-shadow,background-color] duration-200 placeholder:text-[var(--app-placeholder)] disabled:cursor-not-allowed disabled:opacity-55",
          icon ? "ps-16" : "ps-4",
          hasError ? "border-red-500 shadow-[0_0_0_3px_rgb(239_68_68_/_14%)]" : "border-[var(--app-border)]",
          !hasError && focused && "border-[var(--app-ink)] bg-[var(--app-card)] shadow-[0_0_0_3px_color-mix(in_srgb,var(--app-ink)_8%,transparent)]",
          className,
        )}
      />
      <label
        htmlFor={inputId}
        className={cn(
          "pointer-events-none absolute z-10 origin-start select-none bg-[var(--app-card)] px-1.5 text-[var(--app-muted)] transition-all duration-200",
          icon ? "start-16" : "start-3",
          isFloating ? "top-0 -translate-y-1/2 scale-90" : "top-1/2 -translate-y-1/2 scale-100",
          focused && "text-[var(--app-ink)]",
          hasError && "text-red-500",
        )}
      >{label}</label>
      <span aria-live="polite" className={cn("pointer-events-none absolute end-3.5 top-1/2 z-10 grid size-6 -translate-y-1/2 place-items-center text-[var(--app-muted)]", hasError && "text-red-500")}>
        {loading ? <LoaderCircle className="size-4 animate-spin" /> : hasError ? <CircleAlert className="size-4" /> : showSuccess ? <CircleCheck className="size-4 text-[var(--app-ink)]" /> : null}
      </span>
    </div>
    <AnimatePresence initial={false} mode="wait">
      {(resolvedError || helperText) && <motion.p key={resolvedError ? "error" : "helper"} id={messageId} role={resolvedError ? "alert" : undefined} initial={{ opacity: 0, height: 0, y: -5, filter: "blur(4px)" }} animate={{ opacity: 1, height: "auto", y: 0, filter: "blur(0px)" }} exit={{ opacity: 0, height: 0, y: -4, filter: "blur(3px)" }} transition={{ duration: 0.24, ease: [0.22, 1, 0.36, 1] }} className={cn("mt-2 overflow-hidden px-1 text-xs leading-5", resolvedError ? "text-red-500" : "text-[var(--app-muted)]")}>{resolvedError ?? helperText}</motion.p>}
    </AnimatePresence>
  </div>;
});

FloatingLabelInput.displayName = "FloatingLabelInput";
