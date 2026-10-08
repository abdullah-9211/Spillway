import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { axe } from "vitest-axe";
import type { Role } from "@/lib/api";
import { RoleGate, RoleProvider, useRole } from "@/lib/role";

function Probe() {
  return <span>role:{useRole()}</span>;
}

const withRole = (role: Role, ui: React.ReactNode) => render(<RoleProvider role={role}>{ui}</RoleProvider>);

describe("useRole", () => {
  it("returns the provided role", () => {
    withRole("viewer", <Probe />);
    expect(screen.getByText("role:viewer")).toBeInTheDocument();
  });

  it("throws outside a provider, so a missing provider is caught early", () => {
    const spy = console.error;
    console.error = () => {};
    expect(() => render(<Probe />)).toThrow(/RoleProvider/);
    console.error = spy;
  });
});

describe("RoleGate", () => {
  const controls = (
    <>
      <button>Revoke</button>
      <input aria-label="Budget" />
    </>
  );

  it("leaves the controls alone for an admin", () => {
    withRole("admin", <RoleGate allow="admin">{controls}</RoleGate>);
    expect(screen.getByRole("button", { name: "Revoke" })).toBeEnabled();
    expect(screen.getByLabelText("Budget")).toBeEnabled();
    expect(screen.queryByText(/only admins/i)).not.toBeInTheDocument();
  });

  it("disables every control for a viewer and says why", () => {
    withRole("viewer", <RoleGate allow="admin">{controls}</RoleGate>);
    expect(screen.getByRole("button", { name: "Revoke" })).toBeDisabled();
    expect(screen.getByLabelText("Budget")).toBeDisabled();
    expect(screen.getByText("Only admins can do this.")).toBeInTheDocument();
  });

  it("uses a custom reason", () => {
    withRole("viewer", <RoleGate allow="admin" reason="Viewers cannot revoke keys.">{controls}</RoleGate>);
    expect(screen.getByText("Viewers cannot revoke keys.")).toBeInTheDocument();
  });

  it("lets a viewer use controls open to viewers", () => {
    withRole("viewer", <RoleGate allow="viewer">{controls}</RoleGate>);
    expect(screen.getByRole("button", { name: "Revoke" })).toBeEnabled();
  });

  it("has no accessibility violations in the disabled state", async () => {
    const { container } = withRole("viewer", <RoleGate allow="admin">{controls}</RoleGate>);
    expect(await axe(container)).toHaveNoViolations();
  });
});
