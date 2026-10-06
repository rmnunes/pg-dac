import { Component, type ErrorInfo, type ReactNode } from "react";

interface State {
  error?: Error;
}

/** Catches render errors so a component crash shows a message instead of a
 *  blank window, and logs details to the devtools console. */
export class ErrorBoundary extends Component<{ children: ReactNode }, State> {
  state: State = {};

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error("pg-dac UI error:", error, info.componentStack);
  }

  render() {
    if (this.state.error) {
      return (
        <div className="error-boundary">
          <h2>Something went wrong rendering the UI</h2>
          <pre>{String(this.state.error.stack ?? this.state.error)}</pre>
          <button onClick={() => this.setState({ error: undefined })}>Dismiss</button>
        </div>
      );
    }
    return this.props.children;
  }
}
