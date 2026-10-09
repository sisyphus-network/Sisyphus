"use client";

import { useState } from "react";

// Keep the selected record available until the overlay finishes closing.
export function useDialogValue<T>(value: T | null) {
  const [retainedValue, setRetainedValue] = useState(value);
  if (value !== null && value !== retainedValue) setRetainedValue(value);

  return {
    displayedValue: value ?? retainedValue,
    onAfterClose: () => {
      if (value === null) setRetainedValue(null);
    },
  };
}
