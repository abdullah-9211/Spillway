import { BrandMark } from "./icons";
import { LoginForm } from "./LoginForm";
import { ThemeSwitch } from "./ThemeSwitch";

/** The sign-in screen, with the design's step-path background. */
export function LoginCard() {
  return (
    <div className="login">
      <svg className="login__steps" viewBox="0 0 1440 900" preserveAspectRatio="xMidYMid slice" fill="none" aria-hidden="true">
        <path d="M-40 250H320V430H720V610H1120V790H1500" stroke="var(--line2)" strokeWidth="2" opacity=".7" />
        <path d="M-40 290H280V470H680V650H1080V830H1500" stroke="var(--line)" strokeWidth="1.5" />
      </svg>
      <div className="login__theme">
        <ThemeSwitch />
      </div>
      <main className="login__card">
        <div className="brand brand--login">
          <BrandMark size={24} />
          Spillway
        </div>
        <div>
          <h1 className="h1">Sign in</h1>
          <p className="login__sub">Watch runs, check spend and manage keys.</p>
        </div>
        <LoginForm />
        <p className="small">Admins can change keys and approve runs. Viewers can read everything and change nothing.</p>
      </main>
    </div>
  );
}
