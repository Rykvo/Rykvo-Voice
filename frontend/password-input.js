document.addEventListener("click", (event) => {
  const button = event.target.closest("[data-secret-toggle]");
  if (!button) return;
  const input = document.getElementById(button.dataset.secretToggle);
  const visible = input.type === "password";
  input.type = visible ? "text" : "password";
  button.setAttribute("aria-pressed", String(visible));
  button.setAttribute(
    "aria-label",
    `${visible ? "隐藏" : "显示"}${input.labels[0].textContent}`,
  );
});
