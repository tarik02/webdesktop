import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "#/app.tsx";
import { TooltipProvider } from "#/components/ui/tooltip.tsx";
import "#/styles/globals.css";

const root = document.getElementById("root");
if (!root) {
  throw new Error("root element is missing");
}

createRoot(root).render(
  <StrictMode>
    <TooltipProvider>
      <App />
    </TooltipProvider>
  </StrictMode>,
);
